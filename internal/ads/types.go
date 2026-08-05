package ads

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Bid contains an active bid. PriceMicros is CPM in integer micros.
type Bid struct {
	ID             string   `json:"id"`
	Revision       uint64   `json:"revision"`
	PlacementID    string   `json:"placement_id"`
	CampaignID     string   `json:"campaign_id"`
	CreativeID     string   `json:"creative_id"`
	Markup         string   `json:"markup"`
	ClickURL       string   `json:"click_url"`
	PriceMicros    int64    `json:"price_micros"`
	FloorMicros    int64    `json:"floor_micros,omitempty"`
	DealID         string   `json:"deal_id,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	PacingBPS      int64    `json:"pacing_bps,omitempty"`
	Countries      []string `json:"countries,omitempty"`
	Devices        []string `json:"devices,omitempty"`
	ActiveFromUnix int64    `json:"active_from_unix,omitempty"`
	ActiveToUnix   int64    `json:"active_to_unix,omitempty"`
}

func (b Bid) Validate() error {
	return b.validate(true)
}

// ValidateDynamic validates a DSP bid without a revision.
func (b Bid) ValidateDynamic() error {
	return b.validate(false)
}

func (b Bid) validate(requireRevision bool) error {
	if b.ID == "" || (requireRevision && b.Revision == 0) || b.PlacementID == "" || b.CampaignID == "" || b.CreativeID == "" {
		return errors.New("id, revision, placement_id, campaign_id, and creative_id are required")
	}
	if (requireRevision && b.Revision > math.MaxInt64) || len(b.ID) > 128 || len(b.PlacementID) > 128 || strings.ContainsAny(b.PlacementID, "{}") || len(b.Markup) > 64<<10 || len(b.ClickURL) > 4096 || len(b.DealID) > 128 {
		return errors.New("bid field exceeds maximum length")
	}
	if b.PriceMicros <= 0 || b.FloorMicros < 0 || b.PriceMicros < b.FloorMicros {
		return errors.New("price_micros must be positive and meet floor_micros")
	}
	if b.ActiveToUnix != 0 && b.ActiveFromUnix > b.ActiveToUnix {
		return errors.New("active_from_unix is after active_to_unix")
	}
	if b.Priority < 0 || b.Priority > 1000 || (b.Priority > 0 && b.DealID == "") {
		return errors.New("priority must be 0..1000 and requires deal_id when non-zero")
	}
	if b.PacingBPS < 1 || b.PacingBPS > 10_000 {
		return errors.New("pacing_bps must be 1..10000")
	}
	return nil
}

// Normalize sets the persisted default values.
func (b *Bid) Normalize() {
	if b.PacingBPS == 0 {
		b.PacingBPS = 10_000
	}
}

type AuctionRequest struct {
	PlacementID string `json:"placement_id"`
	UserID      string `json:"user_id,omitempty"`
	Country     string `json:"country,omitempty"`
	Device      string `json:"device,omitempty"`
	FloorMicros int64  `json:"floor_micros,omitempty"`
}

func (r AuctionRequest) Validate() error {
	if r.PlacementID == "" || len(r.PlacementID) > 128 || len(r.UserID) > 256 || len(r.Country) > 16 || len(r.Device) > 64 || r.FloorMicros < 0 {
		return errors.New("invalid auction request field")
	}
	return nil
}

type AuctionResponse struct {
	RequestID            string `json:"request_id"`
	Status               string `json:"status"`
	PricingModel         string `json:"pricing_model"`
	Bid                  *Bid   `json:"bid,omitempty"`
	EffectivePriceMicros int64  `json:"effective_price_micros,omitempty"`
	ClearingPriceMicros  int64  `json:"clearing_price_micros,omitempty"`
	NoBidReason          string `json:"no_bid_reason,omitempty"`
	ServedStale          bool   `json:"served_stale,omitempty"`
	SnapshotAgeMS        int64  `json:"snapshot_age_ms,omitempty"`
	DynamicBidderID      string `json:"dynamic_bidder_id,omitempty"`
	SelectionLatencyUS   int64  `json:"selection_latency_us"`
}

type PricingModel string

const (
	FirstPrice  PricingModel = "first_price"
	SecondPrice PricingModel = "second_price"
)

type AuctionPolicy struct {
	PricingModel               PricingModel
	SecondPriceIncrementMicros int64
}

func DefaultAuctionPolicy() AuctionPolicy {
	return AuctionPolicy{PricingModel: FirstPrice, SecondPriceIncrementMicros: 1}
}

func (p AuctionPolicy) Validate() error {
	if p.PricingModel != FirstPrice && p.PricingModel != SecondPrice {
		return errors.New("pricing model must be first_price or second_price")
	}
	if p.SecondPriceIncrementMicros < 0 {
		return errors.New("second price increment must be non-negative")
	}
	return nil
}

type Config struct {
	HTTPAddr            string
	DebugAddr           string
	RedisAddr           string
	RedisPassword       string
	RedisDB             int
	RedisMode           string
	RedisAddrs          []string
	RedisPrefix         string
	AdminToken          string
	CacheRefresh        time.Duration
	AuctionPolicy       AuctionPolicy
	DecisionLogging     bool
	DecisionLogBuffer   int
	MaxSnapshotAge      time.Duration
	StaleSnapshotAction StaleSnapshotAction
	DSPBidderEndpoints  []DSPBidderEndpoint
	DSPAuthorization    string
	BidderEngineConfig  BidderEngineConfig
	DSPTimeout          time.Duration
	DSPMaxConcurrent    int
	DSPFailureThreshold int
	DSPCircuitOpenFor   time.Duration
}

type DSPBidderEndpoint struct {
	ID  string
	URL string
}

func ConfigFromEnv() Config {
	c := Config{HTTPAddr: ":8080", RedisAddr: "127.0.0.1:6379", RedisMode: "single", RedisPrefix: "adserver", CacheRefresh: 30 * time.Second, AuctionPolicy: DefaultAuctionPolicy(), DecisionLogBuffer: 4096, MaxSnapshotAge: time.Minute, StaleSnapshotAction: ServeStale, BidderEngineConfig: DefaultBidderEngineConfig(), DSPTimeout: 60 * time.Millisecond, DSPMaxConcurrent: 32, DSPFailureThreshold: 3, DSPCircuitOpenFor: 5 * time.Second}
	if value := strings.TrimSpace(getenv("HTTP_ADDR")); value != "" {
		c.HTTPAddr = value
	}
	if value := strings.TrimSpace(getenv("DEBUG_ADDR")); value != "" {
		c.DebugAddr = value
	}
	if value := strings.TrimSpace(getenv("REDIS_ADDR")); value != "" {
		c.RedisAddr = value
	}
	if value := strings.TrimSpace(getenv("REDIS_MODE")); value != "" {
		c.RedisMode = value
	}
	if value := strings.TrimSpace(getenv("REDIS_ADDRS")); value != "" {
		for _, address := range strings.Split(value, ",") {
			if address = strings.TrimSpace(address); address != "" {
				c.RedisAddrs = append(c.RedisAddrs, address)
			}
		}
	}
	if value := getenv("REDIS_PASSWORD"); value != "" {
		c.RedisPassword = value
	}
	if value := strings.TrimSpace(getenv("REDIS_DB")); value != "" {
		if db, err := strconv.Atoi(value); err == nil && db >= 0 {
			c.RedisDB = db
		}
	}
	if value := strings.TrimSpace(getenv("REDIS_PREFIX")); value != "" {
		c.RedisPrefix = value
	}
	if value := getenv("AD_ADMIN_TOKEN"); value != "" {
		c.AdminToken = value
	}
	if value := strings.TrimSpace(getenv("CACHE_REFRESH_INTERVAL")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.CacheRefresh = d
		}
	}
	if value := strings.TrimSpace(getenv("MAX_SNAPSHOT_AGE")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.MaxSnapshotAge = d
		}
	}
	if value := strings.TrimSpace(getenv("STALE_SNAPSHOT_ACTION")); value != "" {
		c.StaleSnapshotAction = StaleSnapshotAction(value)
	}
	if value := strings.TrimSpace(getenv("AUCTION_PRICING_MODEL")); value != "" {
		c.AuctionPolicy.PricingModel = PricingModel(value)
	}
	if value := strings.TrimSpace(getenv("SECOND_PRICE_INCREMENT_MICROS")); value != "" {
		if increment, err := strconv.ParseInt(value, 10, 64); err == nil && increment >= 0 {
			c.AuctionPolicy.SecondPriceIncrementMicros = increment
		}
	}
	if value := strings.TrimSpace(getenv("DECISION_LOGGING")); value == "true" {
		c.DecisionLogging = true
	}
	if value := strings.TrimSpace(getenv("DECISION_LOG_BUFFER")); value != "" {
		if size, err := strconv.Atoi(value); err == nil && size > 0 {
			c.DecisionLogBuffer = size
		}
	}
	if value := strings.TrimSpace(getenv("DSP_ENDPOINTS")); value != "" {
		for _, rawEndpoint := range strings.Split(value, ",") {
			parts := strings.SplitN(strings.TrimSpace(rawEndpoint), "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				c.DSPBidderEndpoints = append(c.DSPBidderEndpoints, DSPBidderEndpoint{})
				continue
			}
			c.DSPBidderEndpoints = append(c.DSPBidderEndpoints, DSPBidderEndpoint{ID: parts[0], URL: parts[1]})
		}
	}
	c.DSPAuthorization = getenv("DSP_AUTHORIZATION")
	if value := strings.TrimSpace(getenv("DSP_TIMEOUT")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.DSPTimeout = d
		}
	}
	if value := strings.TrimSpace(getenv("DSP_CACHE_TTL")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.BidderEngineConfig.CacheTTL = d
		}
	}
	if value := strings.TrimSpace(getenv("DSP_FAILURE_TTL")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.BidderEngineConfig.FailureTTL = d
		}
	}
	if value := strings.TrimSpace(getenv("DSP_MAX_FANOUT")); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			c.BidderEngineConfig.MaxFanout = n
		}
	}
	if value := strings.TrimSpace(getenv("DSP_MAX_REFRESHES")); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			c.BidderEngineConfig.MaxConcurrentRefreshes = n
		}
	}
	if value := strings.TrimSpace(getenv("DSP_MAX_KEYS")); value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			c.BidderEngineConfig.MaxKeys = n
		}
	}
	if value := strings.TrimSpace(getenv("DSP_MAX_CONCURRENT")); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			c.DSPMaxConcurrent = n
		}
	}
	if value := strings.TrimSpace(getenv("DSP_FAILURE_THRESHOLD")); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			c.DSPFailureThreshold = n
		}
	}
	if value := strings.TrimSpace(getenv("DSP_CIRCUIT_OPEN")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.DSPCircuitOpenFor = d
		}
	}
	return c
}

func (c Config) Validate() error {
	if err := c.AuctionPolicy.Validate(); err != nil {
		return err
	}
	if c.RedisMode != "single" && c.RedisMode != "cluster" {
		return errors.New("REDIS_MODE must be single or cluster")
	}
	if c.RedisMode == "cluster" && c.RedisDB != 0 {
		return errors.New("Redis Cluster only supports database 0")
	}
	if c.MaxSnapshotAge <= 0 {
		return errors.New("MAX_SNAPSHOT_AGE must be positive")
	}
	if c.StaleSnapshotAction != ServeStale && c.StaleSnapshotAction != NoBidOnStale {
		return errors.New("STALE_SNAPSHOT_ACTION must be serve_stale or no_bid")
	}
	if len(c.DSPBidderEndpoints) > 64 {
		return errors.New("DSP_ENDPOINTS has more than 64 entries")
	}
	for _, endpoint := range c.DSPBidderEndpoints {
		if !validBidderID(endpoint.ID) || endpoint.URL == "" || len(endpoint.URL) > 4096 {
			return errors.New("DSP_ENDPOINTS entries require id=url")
		}
	}
	if len(c.DSPAuthorization) > 8192 || c.DSPTimeout <= 0 || c.DSPTimeout > 10*time.Second || c.DSPMaxConcurrent < 1 || c.DSPMaxConcurrent > 1024 || c.DSPFailureThreshold < 1 || c.DSPFailureThreshold > 100 || c.DSPCircuitOpenFor <= 0 || c.DSPCircuitOpenFor > time.Hour {
		return errors.New("invalid DSP timeout, concurrency, or circuit configuration")
	}
	if err := c.BidderEngineConfig.Validate(); err != nil {
		return err
	}
	return nil
}

func getenv(key string) string { return strings.TrimSpace(os.Getenv(key)) }
