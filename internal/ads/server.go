package ads

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Server struct {
	config         Config
	store          BidStore
	cache          *BidCache
	auctioneer     *Auctioneer
	metrics        Metrics
	logger         *slog.Logger
	decisionLogger *DecisionLogger
	bidderEngine   *BidderEngine
	requestNumber  atomic.Uint64
}

func NewServer(config Config, store BidStore, cache *BidCache, logger *slog.Logger) *Server {
	return &Server{
		config: config, store: store, cache: cache,
		auctioneer: NewAuctioneerWithPolicy(cache, config.AuctionPolicy), logger: logger,
		decisionLogger: NewDecisionLogger(config.DecisionLogging, config.DecisionLogBuffer, logger),
	}
}

func (s *Server) Close() { s.decisionLogger.Close() }

func (s *Server) SetBidderEngine(engine *BidderEngine) { s.bidderEngine = engine }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auction", s.auction)
	mux.HandleFunc("PUT /v1/bids/{id}", s.upsertBid)
	mux.HandleFunc("DELETE /v1/bids/{id}", s.deleteBid)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.metricsEndpoint)
	return securityHeaders(mux)
}

func (s *Server) auction(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	filled := false
	selectionLatency := int64(0)
	valid := false
	defer func() {
		if valid {
			s.metrics.RecordAuction(filled, selectionLatency, time.Since(started).Microseconds())
		}
	}()
	var request AuctionRequest
	if !decodeJSON(w, r, &request) || request.Validate() != nil {
		s.metrics.badRequests.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid auction request"})
		return
	}
	selectionStarted := time.Now()
	var dynamic []DynamicBid
	if s.bidderEngine != nil {
		dynamic, _ = s.bidderEngine.Lookup(request)
		s.bidderEngine.Trigger(request)
	}
	decision := s.auctioneer.RunWithDynamic(request, dynamic)
	filled = decision.Filled()
	selectionLatency = time.Since(selectionStarted).Microseconds()
	valid = true
	requestID := s.requestID(r)
	response := AuctionResponse{
		RequestID: requestID, Status: "no_bid", PricingModel: string(s.auctioneer.policy.PricingModel),
		NoBidReason: decision.NoBidReason, ServedStale: decision.ServedStale,
		SnapshotAgeMS: decision.SnapshotAge.Milliseconds(), SelectionLatencyUS: selectionLatency,
	}
	if decision.ServedStale {
		s.metrics.servedStale.Add(1)
	}
	if decision.NoBidReason == "stale_snapshot" {
		s.metrics.staleNoBid.Add(1)
	}
	if decision.DynamicBidderID != "" {
		s.metrics.dynamicWins.Add(1)
	}
	if filled {
		response.Status = "filled"
		response.Bid = decision.Bid
		response.EffectivePriceMicros = decision.EffectivePriceMicros
		response.ClearingPriceMicros = decision.ClearingPriceMicros
		response.DynamicBidderID = decision.DynamicBidderID
	}
	s.recordDecision(requestID, request.PlacementID, decision)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) recordDecision(requestID, placementID string, decision AuctionDecision) {
	if s.decisionLogger == nil {
		return
	}
	entry := DecisionLog{
		RequestID: requestID, PlacementID: placementID, PricingModel: s.auctioneer.policy.PricingModel,
		EligibleCandidates: decision.EligibleCandidates, EffectivePriceMicros: decision.EffectivePriceMicros,
		ClearingPriceMicros: decision.ClearingPriceMicros, NoBidReason: decision.NoBidReason,
		ServedStale: decision.ServedStale, SnapshotAgeMS: decision.SnapshotAge.Milliseconds(),
		DynamicBidderID: decision.DynamicBidderID,
	}
	if decision.Bid != nil {
		entry.WinnerBidID, entry.WinnerDealID, entry.WinnerPriority = decision.Bid.ID, decision.Bid.DealID, decision.Bid.Priority
	}
	if !s.decisionLogger.Record(entry) {
		s.metrics.decisionLogDropped.Add(1)
	}
}

func (s *Server) upsertBid(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var bid Bid
	if !decodeJSON(w, r, &bid) {
		s.metrics.badRequests.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bid or bid id"})
		return
	}
	bid.Normalize()
	if bid.ID != r.PathValue("id") || bid.Validate() != nil {
		s.metrics.badRequests.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bid or bid id"})
		return
	}
	s.metrics.bidMutations.Add(1)
	if err := s.store.Upsert(r.Context(), bid); err != nil {
		s.metrics.bidMutationErrors.Add(1)
		if errors.Is(err, ErrStaleBidRevision) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale bid revision"})
			return
		}
		s.logger.Error("bid upsert failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "bid registry unavailable"})
		return
	}
	// Do not wait for Pub/Sub delivery on the writer instance.
	s.cache.RefreshAsync(bid.PlacementID)
	writeJSON(w, http.StatusOK, bid)
}

func (s *Server) deleteBid(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	placement := r.URL.Query().Get("placement_id")
	revision, err := strconv.ParseUint(r.URL.Query().Get("revision"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revision query parameter is required"})
		return
	}
	s.metrics.bidMutations.Add(1)
	if err := s.store.Delete(r.Context(), placement, r.PathValue("id"), revision); err != nil {
		s.metrics.bidMutationErrors.Add(1)
		if errors.Is(err, ErrStaleBidRevision) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale bid revision"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.cache.RefreshAsync(placement)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	s.metrics.readinessChecks.Add(1)
	if err := s.store.Ping(r.Context()); err != nil {
		s.metrics.readinessFailures.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "redis unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	s.metrics.Handler(w, r)
	writeCacheMetrics(w, s.cache.Stats())
	if s.bidderEngine != nil {
		writeBidderMetrics(w, &s.bidderEngine.stats)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.config.AdminToken == "" {
		return true
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(provided) == len(s.config.AdminToken) && subtle.ConstantTimeCompare([]byte(provided), []byte(s.config.AdminToken)) == 1
}

func (s *Server) requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	return "a-" + strconv.FormatUint(s.requestNumber.Add(1), 36)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
