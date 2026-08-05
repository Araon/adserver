# Performance baseline

This baseline measures a local development system. It does not state production capacity.

The client and server ran on one Apple M1 Pro. Client scheduling and loopback traffic affect the result.

## Measurements

| Metric | Includes | Use |
| --- | --- | --- |
| Client percentiles | HTTP, JSON, scheduling, server work, and response transfer. | Compare full local requests. |
| Selection latency | Snapshot reads, filters, ranking, and pricing. | Find auction algorithm regressions. |
| Handler latency | JSON decode, selection, and JSON encode. | Find server application regressions. |

The server histograms use fixed microsecond buckets. The load tester calculates percentiles from client samples.

## Recorded run

This run used one warm Redis bid and 64 closed-loop clients for 10 seconds.

```text
requests=810358 failures=0 rps=81036
p50=502.541µs p95=1.987208ms p99=3.305792ms max=116.501083ms
```

The server reported these bucket limits:

- Selection p99 was not more than 5 microseconds.
- Handler p99 was not more than 1 millisecond.

The CPU profile ran for five seconds. Network and scheduler system calls used most of the sampled CPU time.

`(*Server).auction` used 1.25% of the cumulative sampled CPU time. This result does not support a custom JSON implementation.

## Reproduce the test

Build the two commands:

```sh
go build -o /tmp/adserver ./cmd/adserver
go build -o /tmp/adserver-loadtest ./cmd/loadtest
```

Start a test Redis process:

```sh
redis-server --port 6394 --save '' --appendonly no
```

Start the server with separate HTTP and pprof listeners:

```sh
REDIS_ADDR=127.0.0.1:6394 \
HTTP_ADDR=127.0.0.1:8094 \
DEBUG_ADDR=127.0.0.1:6064 \
/tmp/adserver
```

Add a bid with the example in the README. Then, run the load test:

```sh
/tmp/adserver-loadtest -url http://127.0.0.1:8094/v1/auction -c 64 -duration 30s
```

Collect a CPU profile during the test:

```sh
go tool pprof -top 'http://127.0.0.1:6064/debug/pprof/profile?seconds=30'
```

Read the server metrics:

```sh
curl -s http://127.0.0.1:8094/metrics
```

## Production gate

Run the load generator on separate hardware. Use the expected request rate for each server instance.

Run the test for 30 minutes. Collect CPU and heap profiles.

Repeat the test while Redis restarts. Check that warm auctions follow the configured stale-snapshot policy.
