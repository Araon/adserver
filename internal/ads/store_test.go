package ads

import "testing"

func TestBidRequiresMonotonicRevisionAndSafePlacementKey(t *testing.T) {
	valid := Bid{ID: "bid", Revision: 1, PlacementID: "homepage_top", CampaignID: "campaign", CreativeID: "creative", PriceMicros: 1, PacingBPS: 10_000}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	withoutRevision := valid
	withoutRevision.Revision = 0
	if err := withoutRevision.Validate(); err == nil {
		t.Fatal("expected missing revision to fail validation")
	}
	unsafePlacement := valid
	unsafePlacement.PlacementID = "{other-slot}"
	if err := unsafePlacement.Validate(); err == nil {
		t.Fatal("expected Redis hash-tag injection to fail validation")
	}

	store := NewRedisStore("127.0.0.1:6379", "", 0, "ads")
	defer store.Close()
	if got, want := store.activeKey("homepage_top"), "ads:active:{homepage_top}"; got != want {
		t.Fatalf("active key got %q want %q", got, want)
	}
	if got, want := store.versionKey("homepage_top"), "ads:active-version:{homepage_top}"; got != want {
		t.Fatalf("version key got %q want %q", got, want)
	}
}
