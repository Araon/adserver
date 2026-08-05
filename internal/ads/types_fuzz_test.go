package ads

import (
	"encoding/json"
	"testing"
)

func FuzzBidValidationNeverPanics(f *testing.F) {
	f.Add([]byte(`{"id":"b","revision":1,"placement_id":"p","campaign_id":"c","creative_id":"x","price_micros":1}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		var bid Bid
		if json.Unmarshal(payload, &bid) == nil {
			bid.Normalize()
			_ = bid.Validate()
			_ = effectivePrice(bid)
		}
	})
}

func FuzzAuctionRequestValidationNeverPanics(f *testing.F) {
	f.Add("placement", int64(0))
	f.Add("", int64(-1))
	f.Fuzz(func(t *testing.T, placement string, floor int64) {
		_ = (AuctionRequest{PlacementID: placement, FloorMicros: floor}).Validate()
	})
}
