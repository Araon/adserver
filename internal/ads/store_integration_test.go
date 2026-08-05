package ads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func integrationStore(t *testing.T) *RedisStore {
	t.Helper()
	address := os.Getenv("INTEGRATION_REDIS_ADDR")
	if address == "" {
		t.Skip("set INTEGRATION_REDIS_ADDR to run Redis integration tests")
	}
	store := NewRedisStore(address, "", 0, fmt.Sprintf("adserver:test:%d", time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		_ = store.Close()
		t.Fatalf("Redis is not reachable at %s: %v", address, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		keys, _ := store.client.Keys(ctx, store.prefix+":*").Result()
		if len(keys) > 0 {
			_ = store.client.Del(ctx, keys...).Err()
		}
		_ = store.Close()
	})
	return store
}

func TestRedisStoreRevisionTombstoneAndOrdering(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	low := Bid{ID: "low", Revision: 1, PlacementID: "top", CampaignID: "c", CreativeID: "low", PriceMicros: 100, PacingBPS: 10_000}
	high := Bid{ID: "high", Revision: 1, PlacementID: "top", CampaignID: "c", CreativeID: "high", PriceMicros: 200, PacingBPS: 10_000}
	if err := store.Upsert(ctx, low); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, high); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, high); err != nil {
		t.Fatalf("identical retry should be idempotent: %v", err)
	}
	conflict := high
	conflict.PriceMicros = 300
	if err := store.Upsert(ctx, conflict); !errors.Is(err, ErrStaleBidRevision) {
		t.Fatalf("same revision with different payload got %v", err)
	}
	bids, err := store.Fetch(ctx, "top")
	if err != nil {
		t.Fatal(err)
	}
	if len(bids) != 2 || bids[0].ID != "high" {
		t.Fatalf("unexpected sorted bids: %+v", bids)
	}
	placements, err := store.Placements(ctx)
	if err != nil || len(placements) != 1 || placements[0] != "top" {
		t.Fatalf("unexpected placements=%v err=%v", placements, err)
	}
	if err := store.Delete(ctx, "top", "high", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "top", "high", 2); err != nil {
		t.Fatalf("identical delete retry should be idempotent: %v", err)
	}
	high.Revision = 1
	if err := store.Upsert(ctx, high); !errors.Is(err, ErrStaleBidRevision) {
		t.Fatalf("tombstone allowed resurrection: %v", err)
	}
}

func TestRedisStorePublishesInvalidations(t *testing.T) {
	store := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messages, closeSubscription, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSubscription()
	bid := Bid{ID: "bid", Revision: 1, PlacementID: "slot", CampaignID: "c", CreativeID: "x", PriceMicros: 100, PacingBPS: 10_000}
	if err := store.Upsert(ctx, bid); err != nil {
		t.Fatal(err)
	}
	select {
	case placement := <-messages:
		if placement != "slot" {
			t.Fatalf("got invalidation for %q", placement)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for invalidation")
	}
}
