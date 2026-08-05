package ads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrStaleBidRevision = errors.New("stale bid revision")

// BidStore manages active bids in Redis. BidCache copies the bids to local snapshots.
type BidStore interface {
	Ping(context.Context) error
	Close() error
	Upsert(context.Context, Bid) error
	Delete(context.Context, string, string, uint64) error
	Fetch(context.Context, string) ([]Bid, error)
	Placements(context.Context) ([]string, error)
	Subscribe(context.Context) (<-chan string, func(), error)
}

type RedisStoreOptions struct {
	Addrs    []string
	Password string
	DB       int
	Prefix   string
	Cluster  bool
}

type RedisStore struct {
	client  redis.UniversalClient
	cluster *redis.ClusterClient
	prefix  string
}

func NewRedisStore(addr, password string, db int, prefix string) *RedisStore {
	return NewRedisStoreWithOptions(RedisStoreOptions{Addrs: []string{addr}, Password: password, DB: db, Prefix: prefix})
}

func NewRedisStoreWithOptions(options RedisStoreOptions) *RedisStore {
	addrs := options.Addrs
	if len(addrs) == 0 {
		addrs = []string{"127.0.0.1:6379"}
	}
	if options.Cluster {
		cluster := redis.NewClusterClient(&redis.ClusterOptions{
			// Cache reconciliation reads primary nodes. A replica can return old bid data.
			Addrs: addrs, Password: options.Password, ReadOnly: false,
			MaxRetries: 3, MaxRedirects: 8, MinRetryBackoff: 8 * time.Millisecond, MaxRetryBackoff: 80 * time.Millisecond,
			DialTimeout: 300 * time.Millisecond, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond,
		})
		return &RedisStore{client: cluster, cluster: cluster, prefix: options.Prefix}
	}
	return &RedisStore{client: redis.NewClient(&redis.Options{
		Addr: addrs[0], Password: options.Password, DB: options.DB, PoolSize: 128, MinIdleConns: 8,
		MaxRetries: 3, MinRetryBackoff: 8 * time.Millisecond, MaxRetryBackoff: 80 * time.Millisecond,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond,
	}), prefix: options.Prefix}
}

func (s *RedisStore) key(parts ...string) string        { return s.prefix + ":" + strings.Join(parts, ":") }
func (s *RedisStore) activeKey(placement string) string { return s.key("active", "{"+placement+"}") }
func (s *RedisStore) versionKey(placement string) string {
	return s.key("active-version", "{"+placement+"}")
}
func (s *RedisStore) placementsKey() string { return s.key("placements") }
func (s *RedisStore) eventsKey() string     { return s.key("bid-events") }

func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }
func (s *RedisStore) Close() error                   { return s.client.Close() }

// retryTopology retries an idempotent Redis operation after a primary change.
func (s *RedisStore) retryTopology(ctx context.Context, operation func() error) error {
	err := operation()
	if err == nil || s.cluster == nil {
		return err
	}
	s.cluster.ReloadState(ctx)
	return operation()
}

const upsertBidScript = `
local current = redis.call('HGET', KEYS[2], ARGV[1])
if current then
  local revision = tonumber(ARGV[2])
  local existing = tonumber(current)
  if revision < existing then return 0 end
  if revision == existing then
    if redis.call('HGET', KEYS[1], ARGV[1]) == ARGV[3] then return 2 end
    return 0
  end
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[3])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
return 1`

const deleteBidScript = `
local current = redis.call('HGET', KEYS[2], ARGV[1])
if current then
  local revision = tonumber(ARGV[2])
  local existing = tonumber(current)
  if revision < existing then return 0 end
  if revision == existing then
    if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 0 then return 2 end
    return 0
  end
end
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
return 1`

func (s *RedisStore) Upsert(ctx context.Context, bid Bid) error {
	bid.Normalize()
	if err := bid.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(bid)
	if err != nil {
		return fmt.Errorf("marshal bid: %w", err)
	}
	// Add the placement before publication. New instances can then find missed updates.
	if err := s.retryTopology(ctx, func() error { return s.client.SAdd(ctx, s.placementsKey(), bid.PlacementID).Err() }); err != nil {
		return err
	}
	var result int64
	err = s.retryTopology(ctx, func() error {
		var evalErr error
		result, evalErr = s.client.Eval(ctx, upsertBidScript, []string{s.activeKey(bid.PlacementID), s.versionKey(bid.PlacementID)}, bid.ID, strconv.FormatUint(bid.Revision, 10), payload).Int64()
		return evalErr
	})
	if err != nil {
		return fmt.Errorf("versioned bid upsert: %w", err)
	}
	if result == 0 {
		return ErrStaleBidRevision
	}
	if err := s.retryTopology(ctx, func() error { return s.client.Publish(ctx, s.eventsKey(), bid.PlacementID).Err() }); err != nil {
		return fmt.Errorf("publish bid invalidation: %w", err)
	}
	return nil
}

func (s *RedisStore) Delete(ctx context.Context, placement, bidID string, revision uint64) error {
	if placement == "" || bidID == "" || revision == 0 || revision > uint64(^uint64(0)>>1) {
		return fmt.Errorf("placement_id, bid id, and valid revision are required")
	}
	if err := s.retryTopology(ctx, func() error { return s.client.SAdd(ctx, s.placementsKey(), placement).Err() }); err != nil {
		return err
	}
	var result int64
	err := s.retryTopology(ctx, func() error {
		var evalErr error
		result, evalErr = s.client.Eval(ctx, deleteBidScript, []string{s.activeKey(placement), s.versionKey(placement)}, bidID, strconv.FormatUint(revision, 10)).Int64()
		return evalErr
	})
	if err != nil {
		return fmt.Errorf("versioned bid delete: %w", err)
	}
	if result == 0 {
		return ErrStaleBidRevision
	}
	if err := s.retryTopology(ctx, func() error { return s.client.Publish(ctx, s.eventsKey(), placement).Err() }); err != nil {
		return fmt.Errorf("publish bid invalidation: %w", err)
	}
	return nil
}

func (s *RedisStore) Fetch(ctx context.Context, placement string) ([]Bid, error) {
	var values map[string]string
	err := s.retryTopology(ctx, func() error {
		var fetchErr error
		values, fetchErr = s.client.HGetAll(ctx, s.activeKey(placement)).Result()
		return fetchErr
	})
	if err != nil {
		return nil, err
	}
	bids := make([]Bid, 0, len(values))
	for _, payload := range values {
		var bid Bid
		if err := json.Unmarshal([]byte(payload), &bid); err != nil {
			return nil, fmt.Errorf("decode active bid: %w", err)
		}
		bid.Normalize()
		if err := bid.Validate(); err == nil && bid.PlacementID == placement {
			bids = append(bids, bid)
		}
	}
	sortBids(bids)
	return bids, nil
}

func sortBids(bids []Bid) {
	sort.Slice(bids, func(i, j int) bool {
		if bids[i].Priority != bids[j].Priority {
			return bids[i].Priority > bids[j].Priority
		}
		left, right := effectivePrice(bids[i]), effectivePrice(bids[j])
		if left != right {
			return left > right
		}
		return bids[i].ID < bids[j].ID
	})
}

func (s *RedisStore) Placements(ctx context.Context) ([]string, error) {
	var placements []string
	err := s.retryTopology(ctx, func() error {
		var fetchErr error
		placements, fetchErr = s.client.SMembers(ctx, s.placementsKey()).Result()
		return fetchErr
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(placements)
	return placements, nil
}

func (s *RedisStore) Subscribe(ctx context.Context) (<-chan string, func(), error) {
	pubsub := s.client.Subscribe(ctx, s.eventsKey())
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, err
	}
	out := make(chan string, 128)
	go func() {
		defer close(out)
		for message := range pubsub.Channel() {
			select {
			case out <- message.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, func() { _ = pubsub.Close() }, nil
}
