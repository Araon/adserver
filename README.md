# adserver

adserver selects an ad for an auction request. Redis stores active bids. Each server keeps local bid snapshots for auction traffic.

A warm auction does not call Redis or a DSP. The server ranks bids by priority, effective integer CPM, and bid ID.

## Design

```mermaid
flowchart LR
    Admin["Admin client"] -->|"versioned bid change"| Redis[(Redis)]
    Redis -->|"invalidation"| Cache["Local bid snapshot"]
    Redis -.->|"reconciliation"| Cache

    Client["Auction client"] -->|"POST /v1/auction"| Server["Go server"]
    Cache -->|"local read"| Server
    DSP["DSP endpoints"] -.->|"background refresh"| Dynamic["Local DSP cache"]
    Dynamic -->|"fresh bids"| Server
    Server --> Decision["winner or no_bid"]
```

Redis is the source of truth for active bids. Pub/Sub invalidations tell each server to refresh a placement.

A timer repairs missed invalidations. A Redis failure does not stop a warm snapshot from serving.

DSP calls occur after an auction lookup. A later matching auction can use the result from the local DSP cache.

Read [Architecture](docs/architecture.md) for the data flow, failure behavior, and auction rules.

## Auction rules

The server applies these rules in order:

1. Reject expired bids and bids that do not match the request.
2. Reject bids below the request floor or the bid floor.
3. Select the highest priority.
4. Select the highest effective CPM in that priority.
5. Select the smallest bid ID when two effective CPM values are equal.

The server calculates effective CPM with integer arithmetic:

```text
price_micros * pacing_bps / 10000
```

The default pricing model is first price. Set `AUCTION_PRICING_MODEL=second_price` to use same-priority second-price clearing.

`pacing_bps` is a control-plane value. This service does not calculate pacing from live spend.

## Run the server

Install Go 1.24 and Docker. Then, start Redis:

```sh
docker compose up -d redis
```

Start the server:

```sh
go run ./cmd/adserver
```

Add an active bid:

```sh
curl -X PUT http://127.0.0.1:8080/v1/bids/demo-1 \
  -H 'Content-Type: application/json' \
  -d '{
    "id":"demo-1",
    "revision":1,
    "placement_id":"homepage_top",
    "campaign_id":"spring",
    "creative_id":"creative-a",
    "markup":"<div>demo ad</div>",
    "click_url":"https://example.com",
    "price_micros":1800000,
    "countries":["IN"],
    "devices":["mobile"]
  }'
```

Send an auction request:

```sh
curl -X POST http://127.0.0.1:8080/v1/auction \
  -H 'Content-Type: application/json' \
  -d '{"placement_id":"homepage_top","country":"IN","device":"mobile","floor_micros":1250000}'
```

## HTTP endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/auction` | Select a bid from local snapshots. |
| `PUT` | `/v1/bids/{id}` | Add or replace an active bid. |
| `DELETE` | `/v1/bids/{id}` | Delete a bid and keep its revision tombstone. |
| `GET` | `/healthz` | Report process health. |
| `GET` | `/readyz` | Check Redis for control-plane readiness. |
| `GET` | `/metrics` | Export Prometheus metrics. |

Set `AD_ADMIN_TOKEN` to protect the bid mutation endpoints. Send the token in an `Authorization: Bearer` header.

Each mutation needs a positive revision. Redis rejects an old revision with HTTP `409 Conflict`.

An exact retry is idempotent. A delete keeps a revision tombstone and prevents an old upsert from restoring the bid.

## Dynamic DSP bids

Set `DSP_ENDPOINTS` to enable HTTP DSP adapters:

```sh
DSP_ENDPOINTS='alpha=https://dsp-alpha.example/bid,beta=https://dsp-beta.example/bid'
DSP_AUTHORIZATION='Bearer token-from-the-environment'
DSP_TIMEOUT=60ms
DSP_CACHE_TTL=2s
DSP_FAILURE_TTL=250ms
DSP_MAX_FANOUT=3
DSP_MAX_REFRESHES=128
DSP_MAX_KEYS=10000
DSP_MAX_CONCURRENT=32
DSP_FAILURE_THRESHOLD=3
DSP_CIRCUIT_OPEN=5s
```

The server sends placement, country, device, and floor data. It does not send the user ID.

The local cache key contains the same four fields. This prevents bids for different request floors from sharing one cache entry.

The current auction does not wait for a DSP. It uses a fresh DSP result when one is already in the local cache.

DSP bids use priority `0`. Redis remains the only source for private deals and higher priorities.

The engine limits response size, cache keys, refresh goroutines, fan-out, concurrency, timeouts, and circuit retries.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | HTTP listen address. |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis address in single-node mode. |
| `REDIS_MODE` | `single` | Use `single` or `cluster`. |
| `REDIS_ADDRS` | empty | Comma-separated Redis Cluster addresses. |
| `REDIS_PREFIX` | `adserver` | Redis key prefix. |
| `CACHE_REFRESH_INTERVAL` | `30s` | Placement reconciliation interval. |
| `MAX_SNAPSHOT_AGE` | `1m` | Snapshot age limit. |
| `STALE_SNAPSHOT_ACTION` | `serve_stale` | Use `serve_stale` or `no_bid`. |
| `DECISION_LOGGING` | `false` | Enable bounded decision logs. |
| `DECISION_LOG_BUFFER` | `4096` | Decision-log queue size. |
| `DEBUG_ADDR` | empty | Separate pprof listen address. |

Keep `DEBUG_ADDR` on a private interface. The debug server has no authentication.

## Verification

Run the local gate:

```sh
make verify
```

Run the Redis gate:

```sh
docker compose up -d redis
INTEGRATION_REDIS_ADDR=127.0.0.1:6379 make verify-integration
```

Run the auction benchmark:

```sh
go test -bench=BenchmarkAuction -benchmem ./internal/ads
```

Run a local HTTP load test after you add a bid:

```sh
go run ./cmd/loadtest -c 64 -duration 15s
```

Read [Quality gates](docs/QUALITY_GATES.md) for the CI and monitoring checks.

Read [Performance](docs/PERFORMANCE.md) for the measured development baseline.

Read [Redis resilience](docs/REDIS_RESILIENCE.md) for the version and failover contract.
