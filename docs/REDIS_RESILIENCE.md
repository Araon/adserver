# Redis consistency and resilience

Redis stores the shared active-bid state. A warm auction reads local memory and does not wait for Redis.

## Mutation order

Each mutation has a positive revision for its placement and bid ID.

```text
revision 1: add bid              accepted
revision 2: change bid           accepted
revision 1: delayed retry        rejected
revision 3: delete bid           accepted
revision 2: delayed upsert       rejected
```

A Lua script compares the revision and changes the active bid in one operation.

The active bid hash and revision hash use one Redis Cluster slot. The placement ID provides their shared hash tag.

An exact retry with the accepted revision and payload is idempotent. Redis publishes another invalidation without changing the stored value.

A delete keeps the new revision as a tombstone. Redis rejects an older upsert after the delete.

## Local snapshots

Each server loads the placement index during startup. The server then reads each placement and stores an immutable local snapshot.

Pub/Sub messages start placement refreshes. Scheduled reconciliation repairs missed messages and finds new placements.

| Variable | Default | Purpose |
| --- | --- | --- |
| `CACHE_REFRESH_INTERVAL` | `30s` | Run scheduled reconciliation. |
| `MAX_SNAPSHOT_AGE` | `1m` | Set the snapshot age limit. |
| `STALE_SNAPSHOT_ACTION` | `serve_stale` | Select `serve_stale` or `no_bid`. |

`serve_stale` returns the cached decision and sets `served_stale: true`.

`no_bid` returns `no_bid_reason: "stale_snapshot"`.

Both actions start a background refresh. Neither action reads Redis in the auction path.

## Redis Cluster

Set the cluster addresses:

```sh
REDIS_MODE=cluster
REDIS_ADDRS=redis-a:6379,redis-b:6379,redis-c:6379
```

Cluster mode uses database `0`. The client reads primary nodes because a replica can return an old bid version.

The client reloads the cluster topology after a primary change. It uses bounded retries for idempotent operations.

## Local test record

The following checks passed on July 22, 2026:

- Redis rejected revision 1 after it accepted revision 2.
- A revision 2 delete prevented a revision 1 upsert.
- A warm snapshot served after Redis stopped and the snapshot became stale.
- A six-node Redis Cluster promoted a replica and accepted the next revision.

These checks cover the local implementation. They do not replace a failover test with the selected managed Redis service.

During a failover test, monitor stale results, cache failures, readiness failures, and mutation errors.
