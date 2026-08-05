package ads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBidder struct {
	id    string
	fetch func(context.Context, DSPRequest) ([]Bid, error)
}

func (b fakeBidder) ID() string { return b.id }
func (b fakeBidder) Fetch(ctx context.Context, request DSPRequest) ([]Bid, error) {
	return b.fetch(ctx, request)
}

func testBidderLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for asynchronous bidder work")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDynamicBidRefreshDoesNotBlockAndFallsBackToActiveBid(t *testing.T) {
	static := Bid{ID: "static", Revision: 1, PlacementID: "top", CampaignID: "campaign", CreativeID: "creative", PriceMicros: 100}
	auctioneer := auctioneerForBids(map[string][]Bid{"top": {static}}, DefaultAuctionPolicy())
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{MaxFanout: 1, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10}, []BidderConfig{{Adapter: fakeBidder{id: "dsp", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
		return []Bid{{ID: "dynamic", PlacementID: "top", CampaignID: "dsp-campaign", CreativeID: "dsp-creative", PriceMicros: 200}}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	request := AuctionRequest{PlacementID: "top"}
	started := time.Now()
	initial, _ := engine.Lookup(request)
	engine.Trigger(request)
	if elapsed := time.Since(started); elapsed > 10*time.Millisecond {
		t.Fatalf("trigger waited on bidder for %s", elapsed)
	}
	decision := auctioneer.RunWithDynamic(request, initial)
	if decision.Bid == nil || decision.Bid.ID != "static" {
		t.Fatalf("expected static fallback, got %+v", decision)
	}
	var cached []DynamicBid
	waitFor(t, func() bool { cached, _ = engine.Lookup(request); return len(cached) == 1 })
	decision = auctioneer.RunWithDynamic(request, cached)
	if decision.Bid == nil || decision.Bid.ID != "dsp:dynamic" || decision.DynamicBidderID != "dsp" {
		t.Fatalf("expected dynamic winner, got %+v", decision)
	}
}

func TestServerUsesFreshDynamicBidWithoutWaitingForFirstRefresh(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{"top": {{ID: "static", Revision: 1, PlacementID: "top", CampaignID: "campaign", CreativeID: "creative", PriceMicros: 100}}}}
	cache := NewBidCache(store, testBidderLogger(), time.Hour)
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{MaxFanout: 1, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10}, []BidderConfig{{Adapter: fakeBidder{id: "dsp", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
		return []Bid{{ID: "dynamic", PlacementID: "top", CampaignID: "dsp", CreativeID: "creative", PriceMicros: 200}}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{}, store, cache, testBidderLogger())
	defer server.Close()
	server.SetBidderEngine(engine)
	invoke := func() AuctionResponse {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/auction", bytes.NewBufferString(`{"placement_id":"top"}`))
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
		}
		var response AuctionResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := invoke()
	if first.Bid == nil || first.Bid.ID != "static" || first.DynamicBidderID != "" {
		t.Fatalf("first auction should use fallback: %+v", first)
	}
	waitFor(t, func() bool { bids, _ := engine.Lookup(AuctionRequest{PlacementID: "top"}); return len(bids) == 1 })
	second := invoke()
	if second.Bid == nil || second.Bid.ID != "dsp:dynamic" || second.DynamicBidderID != "dsp" {
		t.Fatalf("second auction should use cache: %+v", second)
	}
}

func TestBidderDeadlineAndCircuitBreaker(t *testing.T) {
	var calls atomic.Uint64
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{MaxFanout: 1, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10}, []BidderConfig{{
		Adapter: fakeBidder{id: "slow", fetch: func(ctx context.Context, _ DSPRequest) ([]Bid, error) {
			calls.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		Timeout: 10 * time.Millisecond, FailureThreshold: 1, CircuitOpenFor: time.Second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine.Trigger(AuctionRequest{PlacementID: "one"})
	waitFor(t, func() bool { return engine.stats.Timeouts.Load() == 1 })
	engine.Trigger(AuctionRequest{PlacementID: "two"})
	waitFor(t, func() bool { return engine.stats.CircuitOpen.Load() == 1 })
	if got := calls.Load(); got != 1 {
		t.Fatalf("open circuit still invoked bidder %d times", got)
	}
}

func TestBidderConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	var active, peak atomic.Int64
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{MaxFanout: 1, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10}, []BidderConfig{{
		Adapter: fakeBidder{id: "limited", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
			current := active.Add(1)
			for {
				previous := peak.Load()
				if current <= previous || peak.CompareAndSwap(previous, current) {
					break
				}
			}
			<-release
			active.Add(-1)
			return nil, errors.New("test failure")
		}}, MaxConcurrent: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine.Trigger(AuctionRequest{PlacementID: "one"})
	waitFor(t, func() bool { return active.Load() == 1 })
	engine.Trigger(AuctionRequest{PlacementID: "two"})
	waitFor(t, func() bool { return engine.stats.FanoutSkipped.Load() == 1 })
	close(release)
	waitFor(t, func() bool { return active.Load() == 0 })
	if peak.Load() != 1 {
		t.Fatalf("max concurrent bidder calls = %d, want 1", peak.Load())
	}
}

func TestDynamicBidCacheReusesAnExpiredSlot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := NewDynamicBidCache(1)
	cache.now = func() time.Time { return now }
	if !cache.Put("first", nil, time.Second) {
		t.Fatal("first cache write failed")
	}
	if cache.Put("second", nil, time.Second) {
		t.Fatal("cache exceeded its key limit")
	}
	now = now.Add(2 * time.Second)
	if !cache.Put("second", nil, time.Second) {
		t.Fatal("cache did not reuse an expired slot")
	}
	if _, ok := cache.Get("first"); ok {
		t.Fatal("expired key remained available")
	}
}

func TestDynamicBidCacheDoesNotExceedKeyLimit(t *testing.T) {
	cache := NewDynamicBidCache(8)
	var writers sync.WaitGroup
	for i := 0; i < 100; i++ {
		writers.Add(1)
		go func(key int) {
			defer writers.Done()
			cache.Put(string(rune(key)), nil, time.Minute)
		}(i)
	}
	writers.Wait()
	keys := 0
	cache.entries.Range(func(_, _ any) bool {
		keys++
		return true
	})
	if keys > 8 {
		t.Fatalf("cache contains %d keys, want at most 8", keys)
	}
}

func TestDynamicBidCacheSeparatesRequestFloors(t *testing.T) {
	low := dynamicCacheKey(AuctionRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 10})
	high := dynamicCacheKey(AuctionRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 20})
	if low == high {
		t.Fatal("cache key does not contain the request floor")
	}
}

func TestBidderEngineBoundsConcurrentRefreshes(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Uint64
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{
		MaxFanout: 1, MaxConcurrentRefreshes: 1, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10,
	}, []BidderConfig{{Adapter: fakeBidder{id: "dsp", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("test failure")
	}}, MaxConcurrent: 2}})
	if err != nil {
		t.Fatal(err)
	}
	engine.Trigger(AuctionRequest{PlacementID: "one"})
	waitFor(t, func() bool { return calls.Load() == 1 })
	engine.Trigger(AuctionRequest{PlacementID: "two"})
	waitFor(t, func() bool { return engine.stats.RefreshSkipped.Load() == 1 })
	close(release)
}

func TestBusyBidderDoesNotLatchHalfOpenCircuit(t *testing.T) {
	var calls atomic.Uint64
	engine, err := NewBidderEngine(context.Background(), BidderEngineConfig{
		MaxFanout: 1, MaxConcurrentRefreshes: 2, CacheTTL: time.Second, FailureTTL: time.Millisecond, MaxKeys: 10,
	}, []BidderConfig{{Adapter: fakeBidder{id: "dsp", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("test failure")
		}
		return nil, nil
	}}, MaxConcurrent: 1, FailureThreshold: 1, CircuitOpenFor: 5 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	engine.Trigger(AuctionRequest{PlacementID: "one"})
	waitFor(t, func() bool { return engine.stats.Failures.Load() == 1 })
	time.Sleep(10 * time.Millisecond)
	engine.bidders[0].sem <- struct{}{}
	engine.Trigger(AuctionRequest{PlacementID: "two"})
	waitFor(t, func() bool { return engine.stats.FanoutSkipped.Load() == 1 })
	<-engine.bidders[0].sem
	engine.Trigger(AuctionRequest{PlacementID: "three"})
	waitFor(t, func() bool { return calls.Load() == 2 })
}

func TestBidderEngineRejectsUnsafeBidderID(t *testing.T) {
	_, err := NewBidderEngine(context.Background(), DefaultBidderEngineConfig(), []BidderConfig{{Adapter: fakeBidder{id: "bad:id", fetch: func(context.Context, DSPRequest) ([]Bid, error) {
		return nil, nil
	}}}})
	if err == nil {
		t.Fatal("expected bidder ID validation error")
	}
}

func BenchmarkAuctionWithFreshDynamicBid(b *testing.B) {
	auctioneer := auctioneerForBids(map[string][]Bid{"top": {{ID: "static", Revision: 1, PlacementID: "top", CampaignID: "campaign", CreativeID: "creative", PriceMicros: 100}}}, DefaultAuctionPolicy())
	request := AuctionRequest{PlacementID: "top"}
	dynamic := []DynamicBid{{Bid: Bid{ID: "dsp:dynamic", PlacementID: "top", CampaignID: "dsp", CreativeID: "creative", PriceMicros: 200, PacingBPS: 10_000}, BidderID: "dsp"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = auctioneer.RunWithDynamic(request, dynamic)
	}
}
