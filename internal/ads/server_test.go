package ads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type faultStore struct {
	*memoryStore
	pingErr   error
	upsertErr error
	deleteErr error
}

func (s *faultStore) Ping(context.Context) error { return s.pingErr }
func (s *faultStore) Upsert(context.Context, Bid) error {
	return s.upsertErr
}
func (s *faultStore) Delete(context.Context, string, string, uint64) error {
	return s.deleteErr
}

func newHTTPTestServer(t *testing.T, config Config, store BidStore) *Server {
	t.Helper()
	cache := NewBidCache(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	server := NewServer(config, store, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(server.Close)
	return server
}

func TestAuctionHandlerReturnsNoBidForColdPlacement(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	cache := NewBidCache(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	server := NewServer(Config{}, store, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "/v1/auction", bytes.NewBufferString(`{"placement_id":"cold"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); !bytes.Contains([]byte(got), []byte(`"no_bid"`)) {
		t.Fatalf("unexpected response: %s", got)
	}
}

func TestAdminTokenProtectsWrites(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	server := newHTTPTestServer(t, Config{AdminToken: "secret"}, store)
	request := httptest.NewRequest(http.MethodPut, "/v1/bids/b1", bytes.NewBufferString(`{"id":"b1","placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", recorder.Code)
	}
}

func TestHealthReadinessAndMetricsEndpoints(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	server := newHTTPTestServer(t, Config{}, store)

	for path, status := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/metrics": http.StatusOK} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != status {
			t.Fatalf("%s: got status %d", path, recorder.Code)
		}
		if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: missing security header", path)
		}
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, metric := range []string{
		"adserver_readiness_checks_total 1",
		"adserver_cache_refresh_total",
		"adserver_cache_placements",
	} {
		if !strings.Contains(recorder.Body.String(), metric) {
			t.Fatalf("metrics missing %q:\n%s", metric, recorder.Body.String())
		}
	}
}

func TestReadinessFailsWhenRedisIsUnavailable(t *testing.T) {
	store := &faultStore{memoryStore: &memoryStore{bids: map[string][]Bid{}}, pingErr: errors.New("down")}
	server := newHTTPTestServer(t, Config{}, store)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestUpsertValidatesJSONAndMapsStoreFailures(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		storeErr   error
		wantStatus int
	}{
		{"valid", `{"id":"b1","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1}`, nil, http.StatusOK},
		{"unknown field", `{"id":"b1","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1,"surprise":true}`, nil, http.StatusBadRequest},
		{"trailing JSON", `{"id":"b1"} {}`, nil, http.StatusBadRequest},
		{"path mismatch", `{"id":"other","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1}`, nil, http.StatusBadRequest},
		{"stale revision", `{"id":"b1","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1}`, ErrStaleBidRevision, http.StatusConflict},
		{"registry unavailable", `{"id":"b1","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"cr","price_micros":1}`, errors.New("down"), http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &faultStore{memoryStore: &memoryStore{bids: map[string][]Bid{}}, upsertErr: test.storeErr}
			server := newHTTPTestServer(t, Config{AdminToken: "secret"}, store)
			request := httptest.NewRequest(http.MethodPut, "/v1/bids/b1", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer secret")
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("got %d want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestDeleteValidatesRevisionAndMapsStoreFailures(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		storeErr   error
		wantStatus int
	}{
		{"valid", "?placement_id=p&revision=2", nil, http.StatusNoContent},
		{"missing revision", "?placement_id=p", nil, http.StatusBadRequest},
		{"stale revision", "?placement_id=p&revision=2", ErrStaleBidRevision, http.StatusConflict},
		{"store failure", "?placement_id=p&revision=2", errors.New("down"), http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &faultStore{memoryStore: &memoryStore{bids: map[string][]Bid{}}, deleteErr: test.storeErr}
			server := newHTTPTestServer(t, Config{}, store)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/bids/b1"+test.query, nil))
			if recorder.Code != test.wantStatus {
				t.Fatalf("got %d want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestAuctionRejectsOversizedBodyAndPreservesRequestID(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	server := newHTTPTestServer(t, Config{}, store)

	oversized := httptest.NewRecorder()
	server.Handler().ServeHTTP(oversized, httptest.NewRequest(http.MethodPost, "/v1/auction", strings.NewReader(strings.Repeat("x", 65<<10))))
	if oversized.Code != http.StatusBadRequest {
		t.Fatalf("oversized body got %d", oversized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/auction", strings.NewReader(`{"placement_id":"cold"}`))
	request.Header.Set("X-Request-ID", "trace-123")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), `"request_id":"trace-123"`) {
		t.Fatalf("request ID not preserved: %s", recorder.Body.String())
	}
}

func TestAuctionRejectsOversizedTargetingFields(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	server := newHTTPTestServer(t, Config{}, store)
	tests := []string{
		`{"placement_id":"p","user_id":"` + strings.Repeat("u", 257) + `"}`,
		`{"placement_id":"p","country":"` + strings.Repeat("c", 17) + `"}`,
		`{"placement_id":"p","device":"` + strings.Repeat("d", 65) + `"}`,
	}
	for _, body := range tests {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/auction", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("oversized targeting field got status %d", recorder.Code)
		}
	}
}
