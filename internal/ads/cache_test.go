package ads

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCacheMarksOldSnapshotStaleAndCanServeIt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryStore{bids: map[string][]Bid{"top": {{ID: "one", Revision: 1, PlacementID: "top", CampaignID: "c", CreativeID: "cr", PriceMicros: 100}}}}
	cache := NewBidCacheWithPolicy(store, slog.New(slog.NewTextHandler(io.Discard, nil)), CachePolicy{RefreshEvery: time.Minute, MaxSnapshotAge: time.Second, StaleAction: ServeStale})
	cache.now = func() time.Time { return now }
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	_, state := cache.Get("top")
	if !state.Found || !state.Stale || state.RejectStale {
		t.Fatalf("unexpected state: %+v", state)
	}

	auctioneer := NewAuctioneer(cache)
	auctioneer.now = func() time.Time { return now }
	decision := auctioneer.Run(AuctionRequest{PlacementID: "top"})
	if !decision.Filled() || !decision.ServedStale {
		t.Fatalf("expected stale fill, got %+v", decision)
	}
}

func TestCacheCanFailClosedWhenSnapshotIsTooOld(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryStore{bids: map[string][]Bid{"top": {{ID: "one", Revision: 1, PlacementID: "top", CampaignID: "c", CreativeID: "cr", PriceMicros: 100}}}}
	cache := NewBidCacheWithPolicy(store, slog.New(slog.NewTextHandler(io.Discard, nil)), CachePolicy{RefreshEvery: time.Minute, MaxSnapshotAge: time.Second, StaleAction: NoBidOnStale})
	cache.now = func() time.Time { return now }
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	decision := NewAuctioneer(cache).Run(AuctionRequest{PlacementID: "top"})
	if decision.Filled() || decision.NoBidReason != "stale_snapshot" {
		t.Fatalf("expected stale no bid, got %+v", decision)
	}
}

func TestReconcileDiscoversPlacementAfterMissedEvent(t *testing.T) {
	store := &memoryStore{bids: map[string][]Bid{}}
	cache := NewBidCache(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute)
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.bids["new-placement"] = []Bid{{ID: "one", Revision: 1, PlacementID: "new-placement", CampaignID: "c", CreativeID: "cr", PriceMicros: 100}}
	store.mu.Unlock()
	if err := cache.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, state := cache.Get("new-placement"); !state.Found {
		t.Fatal("reconciliation did not discover new placement")
	}
}
