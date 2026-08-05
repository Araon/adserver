package ads

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type StaleSnapshotAction string

const (
	ServeStale   StaleSnapshotAction = "serve_stale"
	NoBidOnStale StaleSnapshotAction = "no_bid"
)

type CachePolicy struct {
	RefreshEvery   time.Duration
	MaxSnapshotAge time.Duration
	StaleAction    StaleSnapshotAction
}

func DefaultCachePolicy(refreshEvery time.Duration) CachePolicy {
	if refreshEvery <= 0 {
		refreshEvery = 30 * time.Second
	}
	return CachePolicy{RefreshEvery: refreshEvery, MaxSnapshotAge: 2 * refreshEvery, StaleAction: ServeStale}
}

func (p CachePolicy) Validate() bool {
	return p.RefreshEvery > 0 && p.MaxSnapshotAge > 0 && (p.StaleAction == ServeStale || p.StaleAction == NoBidOnStale)
}

type bidList struct {
	bids          []Bid
	refreshedAtNS int64
}
type placementSnapshot struct{ value atomic.Pointer[bidList] }

type SnapshotState struct {
	Found       bool
	Stale       bool
	RejectStale bool
	Age         time.Duration
}

// BidCache supplies immutable local snapshots. Auction requests do not call BidStore.
type BidCache struct {
	store    BidStore
	logger   *slog.Logger
	entries  sync.Map // map[string]*placementSnapshot
	policy   CachePolicy
	inflight sync.Map // map[string]struct{}
	now      func() time.Time
	stats    cacheCounters
}

type cacheCounters struct {
	refreshes            atomic.Uint64
	refreshFailures      atomic.Uint64
	reconciliations      atomic.Uint64
	reconcileFailures    atomic.Uint64
	subscriptionFailures atomic.Uint64
}

// CacheStats contains counters and gauges with fixed dimensions.
type CacheStats struct {
	Refreshes            uint64
	RefreshFailures      uint64
	Reconciliations      uint64
	ReconcileFailures    uint64
	SubscriptionFailures uint64
	Placements           uint64
	InflightRefreshes    uint64
}

func NewBidCache(store BidStore, logger *slog.Logger, refreshEvery time.Duration) *BidCache {
	return NewBidCacheWithPolicy(store, logger, DefaultCachePolicy(refreshEvery))
}

func NewBidCacheWithPolicy(store BidStore, logger *slog.Logger, policy CachePolicy) *BidCache {
	if !policy.Validate() {
		policy = DefaultCachePolicy(30 * time.Second)
	}
	return &BidCache{store: store, logger: logger, policy: policy, now: time.Now}
}

func (c *BidCache) Warm(ctx context.Context) error { return c.Reconcile(ctx) }

// Reconcile refreshes each known placement. It also finds placements after a missed event.
func (c *BidCache) Reconcile(ctx context.Context) error {
	c.stats.reconciliations.Add(1)
	placements, err := c.store.Placements(ctx)
	if err != nil {
		c.stats.reconcileFailures.Add(1)
		return err
	}
	for _, placement := range placements {
		if err := c.refresh(ctx, placement); err != nil {
			c.stats.reconcileFailures.Add(1)
			return err
		}
	}
	return nil
}

func (c *BidCache) Start(ctx context.Context) {
	go c.subscribeLoop(ctx)
	go c.refreshLoop(ctx)
}

func (c *BidCache) Get(placement string) ([]Bid, SnapshotState) {
	entry, ok := c.entries.Load(placement)
	if !ok {
		return nil, SnapshotState{}
	}
	list := entry.(*placementSnapshot).value.Load()
	if list == nil {
		return nil, SnapshotState{}
	}
	age := c.now().Sub(time.Unix(0, list.refreshedAtNS))
	stale := age > c.policy.MaxSnapshotAge
	return list.bids, SnapshotState{Found: true, Stale: stale, RejectStale: stale && c.policy.StaleAction == NoBidOnStale, Age: age}
}

func (c *BidCache) RefreshAsync(placement string) {
	if placement == "" {
		return
	}
	if _, loaded := c.inflight.LoadOrStore(placement, struct{}{}); loaded {
		return
	}
	go func() {
		defer c.inflight.Delete(placement)
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()
		if err := c.refresh(ctx, placement); err != nil {
			c.logger.Warn("bid snapshot refresh failed", "placement", placement, "error", err)
		}
	}()
}

func (c *BidCache) refresh(ctx context.Context, placement string) error {
	c.stats.refreshes.Add(1)
	bids, err := c.store.Fetch(ctx, placement)
	if err != nil {
		c.stats.refreshFailures.Add(1)
		return err
	}
	for i := range bids {
		bids[i].Normalize()
	}
	sortBids(bids)
	entry, _ := c.entries.LoadOrStore(placement, &placementSnapshot{})
	// Do not change the bids or their backing array after this store.
	entry.(*placementSnapshot).value.Store(&bidList{bids: bids, refreshedAtNS: c.now().UnixNano()})
	return nil
}

func (c *BidCache) Stats() CacheStats {
	var placements, inflight uint64
	c.entries.Range(func(_, _ any) bool {
		placements++
		return true
	})
	c.inflight.Range(func(_, _ any) bool {
		inflight++
		return true
	})
	return CacheStats{
		Refreshes:            c.stats.refreshes.Load(),
		RefreshFailures:      c.stats.refreshFailures.Load(),
		Reconciliations:      c.stats.reconciliations.Load(),
		ReconcileFailures:    c.stats.reconcileFailures.Load(),
		SubscriptionFailures: c.stats.subscriptionFailures.Load(),
		Placements:           placements,
		InflightRefreshes:    inflight,
	}
}

func (c *BidCache) subscribeLoop(ctx context.Context) {
	for ctx.Err() == nil {
		messages, closeSubscription, err := c.store.Subscribe(ctx)
		if err != nil {
			c.stats.subscriptionFailures.Add(1)
			c.logger.Warn("bid invalidation subscription failed", "error", err)
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}
		for placement := range messages {
			c.RefreshAsync(placement)
		}
		closeSubscription()
	}
}

func (c *BidCache) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(c.policy.RefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			if err := c.Reconcile(refreshCtx); err != nil {
				c.logger.Warn("bid snapshot reconciliation failed", "error", err)
			}
			cancel()
		}
	}
}
