package ads

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu   sync.Mutex
	bids map[string][]Bid
}

func (m *memoryStore) Ping(context.Context) error { return nil }
func (m *memoryStore) Close() error               { return nil }
func (m *memoryStore) Upsert(_ context.Context, bid Bid) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bids[bid.PlacementID] = append(m.bids[bid.PlacementID], bid)
	return nil
}
func (m *memoryStore) Delete(context.Context, string, string, uint64) error { return nil }
func (m *memoryStore) Fetch(_ context.Context, placement string) ([]Bid, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Bid(nil), m.bids[placement]...), nil
}
func (m *memoryStore) Placements(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.bids))
	for key := range m.bids {
		keys = append(keys, key)
	}
	return keys, nil
}
func (m *memoryStore) Subscribe(context.Context) (<-chan string, func(), error) {
	ch := make(chan string)
	return ch, func() { close(ch) }, nil
}

func newTestAuctioneer() *Auctioneer {
	return auctioneerForBids(map[string][]Bid{"top": {
		{ID: "high-wrong-country", PlacementID: "top", CampaignID: "a", CreativeID: "a", PriceMicros: 900, Countries: []string{"US"}},
		{ID: "winner", PlacementID: "top", CampaignID: "b", CreativeID: "b", PriceMicros: 800, Countries: []string{"IN"}, Devices: []string{"mobile"}},
		{ID: "low", PlacementID: "top", CampaignID: "c", CreativeID: "c", PriceMicros: 700},
	}}, DefaultAuctionPolicy())
}

func auctioneerForBids(bids map[string][]Bid, policy AuctionPolicy) *Auctioneer {
	store := &memoryStore{bids: bids}
	cache := NewBidCache(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	if err := cache.Warm(context.Background()); err != nil {
		panic(err)
	}
	auctioneer := NewAuctioneerWithPolicy(cache, policy)
	auctioneer.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return auctioneer
}

func TestAuctionSelectsHighestEligibleBid(t *testing.T) {
	auctioneer := newTestAuctioneer()
	bid, filled := auctioneer.Select(AuctionRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 750})
	if !filled || bid.ID != "winner" {
		t.Fatalf("got filled=%t bid=%+v", filled, bid)
	}
}

func TestAuctionRejectsExpiredAndFloorBids(t *testing.T) {
	auctioneer := newTestAuctioneer()
	bid, filled := auctioneer.Select(AuctionRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 801})
	if filled || bid != nil {
		t.Fatalf("expected no bid, got %+v", bid)
	}
}

func TestAuctionUsesDealPriorityAndDeterministicTieBreak(t *testing.T) {
	auctioneer := auctioneerForBids(map[string][]Bid{"top": {
		{ID: "open-high", PlacementID: "top", CampaignID: "open", CreativeID: "a", PriceMicros: 900},
		{ID: "deal-z", PlacementID: "top", CampaignID: "deal", CreativeID: "b", DealID: "reserved", Priority: 10, PriceMicros: 500},
		{ID: "deal-a", PlacementID: "top", CampaignID: "deal", CreativeID: "c", DealID: "reserved", Priority: 10, PriceMicros: 500},
	}}, DefaultAuctionPolicy())
	decision := auctioneer.Run(AuctionRequest{PlacementID: "top"})
	if !decision.Filled() || decision.Bid.ID != "deal-a" || decision.ClearingPriceMicros != 500 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestSecondPriceUsesSamePriorityRunnerUpAndFloor(t *testing.T) {
	policy := AuctionPolicy{PricingModel: SecondPrice, SecondPriceIncrementMicros: 10}
	auctioneer := auctioneerForBids(map[string][]Bid{"top": {
		{ID: "winner", PlacementID: "top", CampaignID: "deal", CreativeID: "a", DealID: "d", Priority: 5, PriceMicros: 900, PacingBPS: 8000},
		{ID: "runner", PlacementID: "top", CampaignID: "deal", CreativeID: "b", DealID: "d", Priority: 5, PriceMicros: 650},
		{ID: "open", PlacementID: "top", CampaignID: "open", CreativeID: "c", PriceMicros: 880},
	}}, policy)
	decision := auctioneer.Run(AuctionRequest{PlacementID: "top", FloorMicros: 600})
	if decision.Bid == nil || decision.Bid.ID != "winner" {
		t.Fatalf("unexpected winner: %+v", decision)
	}
	// The winner has an effective CPM of 720. The lower-priority open bid does not set the clearing price.
	if decision.EffectivePriceMicros != 720 || decision.ClearingPriceMicros != 660 {
		t.Fatalf("unexpected pricing: %+v", decision)
	}
}

func TestAuctionRejectsPacedBidBelowBidFloor(t *testing.T) {
	auctioneer := auctioneerForBids(map[string][]Bid{"top": {
		{ID: "paced", PlacementID: "top", CampaignID: "campaign", CreativeID: "creative", PriceMicros: 1000, FloorMicros: 900, PacingBPS: 8000},
	}}, DefaultAuctionPolicy())
	decision := auctioneer.Run(AuctionRequest{PlacementID: "top"})
	if decision.Filled() || decision.NoBidReason != "no_eligible_bid" {
		t.Fatalf("expected paced bid to miss its floor: %+v", decision)
	}
}

func BenchmarkAuction(b *testing.B) {
	auctioneer := newTestAuctioneer()
	request := AuctionRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 750}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = auctioneer.Select(request)
	}
}
