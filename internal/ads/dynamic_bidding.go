package ads

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DSPRequest contains cacheable auction data. It does not contain a user ID.
type DSPRequest struct {
	PlacementID string `json:"placement_id"`
	Country     string `json:"country,omitempty"`
	Device      string `json:"device,omitempty"`
	FloorMicros int64  `json:"floor_micros,omitempty"`
}

type BidderAdapter interface {
	ID() string
	Fetch(context.Context, DSPRequest) ([]Bid, error)
}

type DynamicBid struct {
	Bid      Bid
	BidderID string
}

type BidderConfig struct {
	Adapter          BidderAdapter
	Timeout          time.Duration
	MaxConcurrent    int
	FailureThreshold int
	CircuitOpenFor   time.Duration
}

type BidderEngineConfig struct {
	MaxFanout              int
	MaxConcurrentRefreshes int
	CacheTTL               time.Duration
	FailureTTL             time.Duration
	MaxKeys                int64
}

func (c BidderEngineConfig) Validate() error {
	if c.MaxFanout < 1 || c.MaxFanout > 64 || c.MaxConcurrentRefreshes < 1 || c.MaxConcurrentRefreshes > 4096 {
		return errors.New("invalid DSP fan-out or refresh limit")
	}
	if c.MaxKeys < 1 || c.MaxKeys > 1_000_000 || c.CacheTTL <= 0 || c.CacheTTL > 10*time.Minute || c.FailureTTL <= 0 || c.FailureTTL > time.Minute {
		return errors.New("invalid DSP cache configuration")
	}
	return nil
}

func DefaultBidderEngineConfig() BidderEngineConfig {
	return BidderEngineConfig{MaxFanout: 3, MaxConcurrentRefreshes: 128, CacheTTL: 2 * time.Second, FailureTTL: 250 * time.Millisecond, MaxKeys: 10_000}
}

type dynamicBidList struct {
	bids      []DynamicBid
	expiresNS int64
}
type DynamicBidCache struct {
	entries sync.Map // map[string]*dynamicBidList
	writeMu sync.Mutex
	count   int64
	maxKeys int64
	now     func() time.Time
}

func NewDynamicBidCache(maxKeys int64) *DynamicBidCache {
	if maxKeys < 1 {
		maxKeys = DefaultBidderEngineConfig().MaxKeys
	}
	return &DynamicBidCache{maxKeys: maxKeys, now: time.Now}
}

func (c *DynamicBidCache) Get(key string) ([]DynamicBid, bool) {
	entry, ok := c.entries.Load(key)
	if !ok {
		return nil, false
	}
	list := entry.(*dynamicBidList)
	if c.now().UnixNano() >= list.expiresNS {
		return nil, false
	}
	return list.bids, true
}

func (c *DynamicBidCache) Put(key string, bids []DynamicBid, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	list := &dynamicBidList{bids: append([]DynamicBid(nil), bids...), expiresNS: c.now().Add(ttl).UnixNano()}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, ok := c.entries.Load(key); ok {
		c.entries.Store(key, list)
		return true
	}
	if c.count >= c.maxKeys {
		now := c.now().UnixNano()
		c.entries.Range(func(storedKey, storedValue any) bool {
			if storedValue.(*dynamicBidList).expiresNS <= now {
				c.entries.Delete(storedKey)
				c.count--
			}
			return c.count >= c.maxKeys
		})
	}
	if c.count >= c.maxKeys {
		return false
	}
	c.entries.Store(key, list)
	c.count++
	return true
}

type circuitBreaker struct {
	mu            sync.Mutex
	failures      int
	openUntil     time.Time
	probeInFlight bool
	threshold     int
	openFor       time.Duration
}

func newCircuitBreaker(threshold int, openFor time.Duration) *circuitBreaker {
	if threshold < 1 {
		threshold = 3
	}
	if openFor <= 0 {
		openFor = 5 * time.Second
	}
	return &circuitBreaker{threshold: threshold, openFor: openFor}
}

func (c *circuitBreaker) Allow(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openUntil.IsZero() {
		return true
	}
	if now.Before(c.openUntil) {
		return false
	}
	if c.probeInFlight {
		return false
	}
	c.probeInFlight = true
	return true
}

func (c *circuitBreaker) Success() {
	c.mu.Lock()
	c.failures, c.openUntil, c.probeInFlight = 0, time.Time{}, false
	c.mu.Unlock()
}

func (c *circuitBreaker) Failure(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probeInFlight = false
	c.failures++
	if c.failures >= c.threshold {
		c.openUntil = now.Add(c.openFor)
	}
}

type bidderRuntime struct {
	config  BidderConfig
	sem     chan struct{}
	breaker *circuitBreaker
}

type BidderEngineStats struct {
	CacheHits      atomic.Uint64
	CacheFull      atomic.Uint64
	Refreshes      atomic.Uint64
	RefreshSkipped atomic.Uint64
	FanoutSkipped  atomic.Uint64
	CircuitOpen    atomic.Uint64
	Timeouts       atomic.Uint64
	Failures       atomic.Uint64
}

// BidderEngine gets bids after an auction returns. Auctions read completed results from the local cache.
type BidderEngine struct {
	cache        *DynamicBidCache
	bidders      []*bidderRuntime
	config       BidderEngineConfig
	ctx          context.Context
	refreshSlots chan struct{}
	inflight     sync.Map // map[cache key]struct{}
	stats        BidderEngineStats
}

func NewBidderEngine(ctx context.Context, config BidderEngineConfig, bidderConfigs []BidderConfig) (*BidderEngine, error) {
	if ctx == nil {
		return nil, errors.New("bidder context is required")
	}
	defaults := DefaultBidderEngineConfig()
	if config.MaxFanout < 1 {
		config.MaxFanout = defaults.MaxFanout
	}
	if config.CacheTTL <= 0 {
		config.CacheTTL = defaults.CacheTTL
	}
	if config.FailureTTL <= 0 {
		config.FailureTTL = defaults.FailureTTL
	}
	if config.MaxKeys < 1 {
		config.MaxKeys = defaults.MaxKeys
	}
	if config.MaxConcurrentRefreshes < 1 {
		config.MaxConcurrentRefreshes = defaults.MaxConcurrentRefreshes
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	engine := &BidderEngine{
		cache:        NewDynamicBidCache(config.MaxKeys),
		config:       config,
		ctx:          ctx,
		refreshSlots: make(chan struct{}, config.MaxConcurrentRefreshes),
	}
	seen := make(map[string]struct{}, len(bidderConfigs))
	for _, config := range bidderConfigs {
		if config.Adapter == nil || !validBidderID(config.Adapter.ID()) {
			return nil, errors.New("bidder adapter and id are required")
		}
		if _, exists := seen[config.Adapter.ID()]; exists {
			return nil, fmt.Errorf("duplicate bidder id %q", config.Adapter.ID())
		}
		seen[config.Adapter.ID()] = struct{}{}
		if config.Timeout <= 0 {
			config.Timeout = 60 * time.Millisecond
		}
		if config.MaxConcurrent < 1 {
			config.MaxConcurrent = 32
		}
		if config.Timeout > 10*time.Second || config.MaxConcurrent > 1024 || config.FailureThreshold > 100 || config.CircuitOpenFor > time.Hour {
			return nil, fmt.Errorf("invalid limits for bidder %q", config.Adapter.ID())
		}
		engine.bidders = append(engine.bidders, &bidderRuntime{config: config, sem: make(chan struct{}, config.MaxConcurrent), breaker: newCircuitBreaker(config.FailureThreshold, config.CircuitOpenFor)})
	}
	return engine, nil
}

func (e *BidderEngine) Lookup(request AuctionRequest) ([]DynamicBid, bool) {
	bids, ok := e.cache.Get(dynamicCacheKey(request))
	if ok {
		e.stats.CacheHits.Add(1)
	}
	return bids, ok
}

func (e *BidderEngine) Trigger(request AuctionRequest) {
	if len(e.bidders) == 0 || e.ctx.Err() != nil {
		return
	}
	key := dynamicCacheKey(request)
	if _, fresh := e.cache.Get(key); fresh {
		return
	}
	if _, loaded := e.inflight.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	select {
	case e.refreshSlots <- struct{}{}:
	default:
		e.inflight.Delete(key)
		e.stats.RefreshSkipped.Add(1)
		return
	}
	go func() {
		defer func() { <-e.refreshSlots }()
		defer e.inflight.Delete(key)
		e.refresh(key, DSPRequest{PlacementID: request.PlacementID, Country: request.Country, Device: request.Device, FloorMicros: request.FloorMicros})
	}()
}

func dynamicCacheKey(request AuctionRequest) string {
	return request.PlacementID + "\x1f" + request.Country + "\x1f" + request.Device + "\x1f" + strconv.FormatInt(request.FloorMicros, 10)
}

type bidderResult struct {
	bids    []DynamicBid
	success bool
}

func (e *BidderEngine) refresh(key string, request DSPRequest) {
	e.stats.Refreshes.Add(1)
	results := make(chan bidderResult, e.config.MaxFanout)
	var workers sync.WaitGroup
	started := 0
	for _, bidder := range e.bidders {
		if started == e.config.MaxFanout {
			break
		}
		select {
		case bidder.sem <- struct{}{}:
		default:
			e.stats.FanoutSkipped.Add(1)
			continue
		}
		if !bidder.breaker.Allow(time.Now()) {
			<-bidder.sem
			e.stats.CircuitOpen.Add(1)
			continue
		}
		started++
		workers.Add(1)
		go func(runtime *bidderRuntime) {
			defer workers.Done()
			defer func() { <-runtime.sem }()
			ctx, cancel := context.WithTimeout(e.ctx, runtime.config.Timeout)
			defer cancel()
			bids, err := runtime.config.Adapter.Fetch(ctx, request)
			if err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					e.stats.Timeouts.Add(1)
				} else {
					e.stats.Failures.Add(1)
				}
				runtime.breaker.Failure(time.Now())
				results <- bidderResult{}
				return
			}
			runtime.breaker.Success()
			results <- bidderResult{bids: sanitizeDynamicBids(runtime.config.Adapter.ID(), request, bids), success: true}
		}(bidder)
	}
	workers.Wait()
	close(results)
	var bids []DynamicBid
	success := false
	for result := range results {
		if result.success {
			success = true
			bids = append(bids, result.bids...)
		}
	}
	ttl := e.config.FailureTTL
	if success {
		ttl = e.config.CacheTTL
	}
	if !e.cache.Put(key, bids, ttl) {
		e.stats.CacheFull.Add(1)
	}
}

func validBidderID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func sanitizeDynamicBids(bidderID string, request DSPRequest, bids []Bid) []DynamicBid {
	clean := make([]DynamicBid, 0, len(bids))
	for _, bid := range bids {
		bid.Normalize()
		bid.Revision = 0
		// DSP bids use the open-market priority. Redis stores private deals.
		bid.Priority, bid.DealID = 0, ""
		if bid.PlacementID != request.PlacementID {
			continue
		}
		bid.ID = bidderID + ":" + bid.ID
		if err := bid.ValidateDynamic(); err != nil {
			continue
		}
		clean = append(clean, DynamicBid{Bid: bid, BidderID: bidderID})
	}
	return clean
}
