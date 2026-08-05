package ads

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsKeepsOverflowOutOfFiniteBuckets(t *testing.T) {
	var metrics Metrics
	metrics.RecordAuction(true, 1, 20_000)
	recorder := httptest.NewRecorder()
	metrics.Handler(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `adserver_auction_handler_latency_microseconds_bucket{le="10000"} 0`) {
		t.Fatalf("overflow was incorrectly included in finite bucket:\n%s", body)
	}
	if !strings.Contains(body, `adserver_auction_handler_latency_microseconds_bucket{le="+Inf"} 1`) {
		t.Fatalf("missing infinite bucket:\n%s", body)
	}
}

func TestMetricsReportsBothLatencyDomains(t *testing.T) {
	var metrics Metrics
	metrics.RecordAuction(false, 2, 50)
	recorder := httptest.NewRecorder()
	metrics.Handler(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, metric := range []string{
		"adserver_auction_selection_latency_microseconds_count 1",
		"adserver_auction_handler_latency_microseconds_count 1",
		"adserver_no_bid_total 1",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics missing %q:\n%s", metric, body)
		}
	}
}

func TestBidderMetricsReportBoundFailures(t *testing.T) {
	var stats BidderEngineStats
	stats.CacheFull.Add(2)
	stats.RefreshSkipped.Add(3)
	recorder := httptest.NewRecorder()
	writeBidderMetrics(recorder, &stats)
	body := recorder.Body.String()
	for _, metric := range []string{
		"adserver_dynamic_bid_cache_full_total 2",
		"adserver_dynamic_bid_refresh_skipped_total 3",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics missing %q:\n%s", metric, body)
		}
	}
}
