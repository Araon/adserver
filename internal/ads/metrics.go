package ads

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics stores counters and fixed-bucket latency histograms.
type Metrics struct {
	auctions           atomic.Uint64
	filled             atomic.Uint64
	noBid              atomic.Uint64
	badRequests        atomic.Uint64
	bidMutations       atomic.Uint64
	bidMutationErrors  atomic.Uint64
	readinessChecks    atomic.Uint64
	readinessFailures  atomic.Uint64
	decisionLogDropped atomic.Uint64
	servedStale        atomic.Uint64
	staleNoBid         atomic.Uint64
	dynamicWins        atomic.Uint64
	selectionLatency   [12]atomic.Uint64
	handlerLatency     [12]atomic.Uint64
}

var latencyUpperUS = [...]int64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 10000}

func (m *Metrics) RecordAuction(filled bool, selectionUS, handlerUS int64) {
	m.auctions.Add(1)
	if filled {
		m.filled.Add(1)
	} else {
		m.noBid.Add(1)
	}
	observeHistogram(&m.selectionLatency, selectionUS)
	observeHistogram(&m.handlerLatency, handlerUS)
}

func observeHistogram(histogram *[12]atomic.Uint64, latencyUS int64) {
	for i, upper := range latencyUpperUS {
		if latencyUS <= upper {
			histogram[i].Add(1)
			return
		}
	}
}

func (m *Metrics) Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "adserver_auctions_total %d\n", m.auctions.Load())
	fmt.Fprintf(w, "adserver_fills_total %d\n", m.filled.Load())
	fmt.Fprintf(w, "adserver_no_bid_total %d\n", m.noBid.Load())
	fmt.Fprintf(w, "adserver_bad_requests_total %d\n", m.badRequests.Load())
	fmt.Fprintf(w, "adserver_bid_mutations_total %d\n", m.bidMutations.Load())
	fmt.Fprintf(w, "adserver_bid_mutation_errors_total %d\n", m.bidMutationErrors.Load())
	fmt.Fprintf(w, "adserver_readiness_checks_total %d\n", m.readinessChecks.Load())
	fmt.Fprintf(w, "adserver_readiness_failures_total %d\n", m.readinessFailures.Load())
	fmt.Fprintf(w, "adserver_decision_log_dropped_total %d\n", m.decisionLogDropped.Load())
	fmt.Fprintf(w, "adserver_served_stale_total %d\n", m.servedStale.Load())
	fmt.Fprintf(w, "adserver_stale_snapshot_no_bid_total %d\n", m.staleNoBid.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_wins_total %d\n", m.dynamicWins.Load())
	writeHistogram(w, "adserver_auction_selection_latency_microseconds", &m.selectionLatency, m.auctions.Load())
	writeHistogram(w, "adserver_auction_handler_latency_microseconds", &m.handlerLatency, m.auctions.Load())
}

func writeCacheMetrics(w http.ResponseWriter, stats CacheStats) {
	fmt.Fprintf(w, "adserver_cache_refresh_total %d\n", stats.Refreshes)
	fmt.Fprintf(w, "adserver_cache_refresh_failures_total %d\n", stats.RefreshFailures)
	fmt.Fprintf(w, "adserver_cache_reconciliations_total %d\n", stats.Reconciliations)
	fmt.Fprintf(w, "adserver_cache_reconcile_failures_total %d\n", stats.ReconcileFailures)
	fmt.Fprintf(w, "adserver_cache_subscription_failures_total %d\n", stats.SubscriptionFailures)
	fmt.Fprintf(w, "adserver_cache_placements %d\n", stats.Placements)
	fmt.Fprintf(w, "adserver_cache_refresh_inflight %d\n", stats.InflightRefreshes)
}

func writeBidderMetrics(w http.ResponseWriter, stats *BidderEngineStats) {
	fmt.Fprintf(w, "adserver_dynamic_bid_cache_hits_total %d\n", stats.CacheHits.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_cache_full_total %d\n", stats.CacheFull.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_refreshes_total %d\n", stats.Refreshes.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_refresh_skipped_total %d\n", stats.RefreshSkipped.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_fanout_skipped_total %d\n", stats.FanoutSkipped.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_circuit_open_total %d\n", stats.CircuitOpen.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_timeouts_total %d\n", stats.Timeouts.Load())
	fmt.Fprintf(w, "adserver_dynamic_bid_failures_total %d\n", stats.Failures.Load())
}

func writeHistogram(w http.ResponseWriter, name string, histogram *[12]atomic.Uint64, total uint64) {
	var cumulative uint64
	for i, upper := range latencyUpperUS {
		cumulative += histogram[i].Load()
		fmt.Fprintf(w, "%s_bucket{le=\"%d\"} %d\n", name, upper, cumulative)
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, total)
	fmt.Fprintf(w, "%s_count %d\n", name, total)
}
