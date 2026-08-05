package ads

import "slices"

import "time"

type AuctionDecision struct {
	Bid                  *Bid
	EffectivePriceMicros int64
	ClearingPriceMicros  int64
	EligibleCandidates   int
	NoBidReason          string
	ServedStale          bool
	SnapshotAge          time.Duration
	DynamicBidderID      string
}

func (d AuctionDecision) Filled() bool { return d.Bid != nil }

type Auctioneer struct {
	cache  *BidCache
	policy AuctionPolicy
	now    func() time.Time
}

func NewAuctioneer(cache *BidCache) *Auctioneer {
	return NewAuctioneerWithPolicy(cache, DefaultAuctionPolicy())
}

func NewAuctioneerWithPolicy(cache *BidCache, policy AuctionPolicy) *Auctioneer {
	if policy.Validate() != nil {
		policy = DefaultAuctionPolicy()
	}
	return &Auctioneer{cache: cache, policy: policy, now: time.Now}
}

// Run ranks the bids in the local snapshot.
// It uses priority, effective CPM, and bid ID. Second-price clearing uses the winner's priority.
func (a *Auctioneer) Run(request AuctionRequest) AuctionDecision {
	return a.run(request, nil)
}

// RunWithDynamic adds cached DSP bids to the Redis snapshot. It does not call a DSP.
func (a *Auctioneer) RunWithDynamic(request AuctionRequest, dynamic []DynamicBid) AuctionDecision {
	return a.run(request, dynamic)
}

func (a *Auctioneer) run(request AuctionRequest, dynamic []DynamicBid) AuctionDecision {
	bids, snapshot := a.cache.Get(request.PlacementID)
	if !snapshot.Found {
		a.cache.RefreshAsync(request.PlacementID)
		return AuctionDecision{NoBidReason: "cold_cache"}
	}
	if snapshot.Stale {
		a.cache.RefreshAsync(request.PlacementID)
		if snapshot.RejectStale {
			return AuctionDecision{NoBidReason: "stale_snapshot", SnapshotAge: snapshot.Age}
		}
	}
	accumulator := auctionAccumulator{request: request, nowUnix: a.now().Unix()}
	for i := range bids {
		accumulator.consider(&bids[i], "")
	}
	for i := range dynamic {
		accumulator.consider(&dynamic[i].Bid, dynamic[i].BidderID)
	}
	if accumulator.winner == nil {
		return AuctionDecision{EligibleCandidates: accumulator.candidates, NoBidReason: "no_eligible_bid", ServedStale: snapshot.Stale, SnapshotAge: snapshot.Age}
	}
	return AuctionDecision{
		Bid: accumulator.winner, EffectivePriceMicros: accumulator.winnerPrice,
		ClearingPriceMicros: a.clearingPrice(*accumulator.winner, accumulator.winnerPrice, accumulator.runnerUpPrice, request.FloorMicros),
		EligibleCandidates:  accumulator.candidates,
		ServedStale:         snapshot.Stale,
		SnapshotAge:         snapshot.Age,
		DynamicBidderID:     accumulator.winnerBidderID,
	}
}

type auctionAccumulator struct {
	request                    AuctionRequest
	nowUnix                    int64
	winner                     *Bid
	winnerPrice, runnerUpPrice int64
	candidates                 int
	winnerBidderID             string
}

func (a *auctionAccumulator) consider(bid *Bid, bidderID string) {
	price := effectivePrice(*bid)
	if !eligible(*bid, a.request, a.nowUnix, price) {
		return
	}
	a.candidates++
	if a.winner == nil || ranksAhead(*bid, price, *a.winner, a.winnerPrice) {
		if a.winner != nil && bid.Priority == a.winner.Priority {
			a.runnerUpPrice = a.winnerPrice
		} else {
			a.runnerUpPrice = 0
		}
		a.winner, a.winnerPrice, a.winnerBidderID = bid, price, bidderID
		return
	}
	if bid.Priority == a.winner.Priority && price > a.runnerUpPrice {
		a.runnerUpPrice = price
	}
}

// Select returns the winner and the fill status.
func (a *Auctioneer) Select(request AuctionRequest) (*Bid, bool) {
	decision := a.Run(request)
	return decision.Bid, decision.Filled()
}

func (a *Auctioneer) clearingPrice(winner Bid, winnerPrice, runnerUpPrice, requestFloor int64) int64 {
	floor := max(requestFloor, winner.FloorMicros)
	if a.policy.PricingModel == FirstPrice {
		return winnerPrice
	}
	if runnerUpPrice == 0 {
		return floor
	}
	secondPrice := runnerUpPrice + a.policy.SecondPriceIncrementMicros
	if secondPrice < runnerUpPrice || secondPrice > winnerPrice {
		secondPrice = winnerPrice
	}
	return max(floor, secondPrice)
}

func ranksAhead(candidate Bid, candidatePrice int64, winner Bid, winnerPrice int64) bool {
	if candidate.Priority != winner.Priority {
		return candidate.Priority > winner.Priority
	}
	if candidatePrice != winnerPrice {
		return candidatePrice > winnerPrice
	}
	return candidate.ID < winner.ID
}

func effectivePrice(bid Bid) int64 {
	// Divide before multiplication to prevent overflow. This keeps CPM values as integers.
	return bid.PriceMicros/10_000*bid.PacingBPS + bid.PriceMicros%10_000*bid.PacingBPS/10_000
}

func eligible(bid Bid, request AuctionRequest, nowUnix, price int64) bool {
	if price < max(request.FloorMicros, bid.FloorMicros) {
		return false
	}
	if bid.ActiveFromUnix != 0 && nowUnix < bid.ActiveFromUnix {
		return false
	}
	if bid.ActiveToUnix != 0 && nowUnix >= bid.ActiveToUnix {
		return false
	}
	return matches(bid.Countries, request.Country) && matches(bid.Devices, request.Device)
}

func matches(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.Contains(allowed, value)
}
