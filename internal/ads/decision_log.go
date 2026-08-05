package ads

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// DecisionLog contains auction diagnostics.
// It does not contain user IDs or creative markup. Do not use it as a billing record.
type DecisionLog struct {
	RequestID            string
	PlacementID          string
	PricingModel         PricingModel
	EligibleCandidates   int
	WinnerBidID          string
	WinnerDealID         string
	WinnerPriority       int
	EffectivePriceMicros int64
	ClearingPriceMicros  int64
	NoBidReason          string
	ServedStale          bool
	SnapshotAgeMS        int64
	DynamicBidderID      string
}

type DecisionLogger struct {
	logger  *slog.Logger
	entries chan DecisionLog
	done    chan struct{}
	closed  sync.Once
	dropped atomic.Uint64
}

func NewDecisionLogger(enabled bool, buffer int, logger *slog.Logger) *DecisionLogger {
	if !enabled {
		return nil
	}
	if buffer < 1 {
		buffer = 4096
	}
	d := &DecisionLogger{logger: logger, entries: make(chan DecisionLog, buffer), done: make(chan struct{})}
	go d.run()
	return d
}

// Record returns false when the bounded diagnostic queue is full.
func (d *DecisionLogger) Record(entry DecisionLog) bool {
	if d == nil {
		return true
	}
	select {
	case d.entries <- entry:
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

func (d *DecisionLogger) Dropped() uint64 {
	if d == nil {
		return 0
	}
	return d.dropped.Load()
}

func (d *DecisionLogger) Close() {
	if d == nil {
		return
	}
	d.closed.Do(func() {
		close(d.entries)
		<-d.done
	})
}

func (d *DecisionLogger) run() {
	defer close(d.done)
	for entry := range d.entries {
		d.logger.Info("auction decision",
			"request_id", entry.RequestID,
			"placement_id", entry.PlacementID,
			"pricing_model", entry.PricingModel,
			"eligible_candidates", entry.EligibleCandidates,
			"winner_bid_id", entry.WinnerBidID,
			"winner_deal_id", entry.WinnerDealID,
			"winner_priority", entry.WinnerPriority,
			"effective_price_micros", entry.EffectivePriceMicros,
			"clearing_price_micros", entry.ClearingPriceMicros,
			"no_bid_reason", entry.NoBidReason,
			"served_stale", entry.ServedStale,
			"snapshot_age_ms", entry.SnapshotAgeMS,
			"dynamic_bidder_id", entry.DynamicBidderID,
		)
	}
}
