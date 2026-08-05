package ads

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPBidderAdapterPostsCohortRequest(t *testing.T) {
	adapter, err := NewHTTPBidderAdapter("dsp", "https://dsp.example/bid", "Bearer token")
	if err != nil {
		t.Fatal(err)
	}
	adapter.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing authorization")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"placement_id":"top","country":"IN","device":"mobile","floor_micros":20}` {
			t.Errorf("unexpected request %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"bids":[{"id":"d","placement_id":"top","campaign_id":"c","creative_id":"cr","price_micros":100}]}`)),
		}, nil
	})}
	bids, err := adapter.Fetch(context.Background(), DSPRequest{PlacementID: "top", Country: "IN", Device: "mobile", FloorMicros: 20})
	if err != nil || len(bids) != 1 || bids[0].ID != "d" {
		t.Fatalf("bids=%+v err=%v", bids, err)
	}
}

func TestHTTPBidderAdapterRejectsOversizedResponse(t *testing.T) {
	adapter, err := NewHTTPBidderAdapter("dsp", "https://dsp.example/bid", "")
	if err != nil {
		t.Fatal(err)
	}
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxDSPResponseBytes+1))),
		}, nil
	})}
	if _, err := adapter.Fetch(context.Background(), DSPRequest{PlacementID: "top"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected response-size error, got %v", err)
	}
}
