# Contributing

Keep each change small. Add tests with the behavior that they verify.

## Service rules

- Keep Redis, DSP calls, and logging out of the warm auction path.
- Rank bids by priority, effective integer CPM, and bid ID.
- Store money in integer micros.
- Preserve monotonic revisions, exact retries, and delete tombstones.
- Set limits for bodies, queues, retries, timeouts, labels, and goroutines.
- Do not log user IDs, creative markup, authorization headers, passwords, or tokens.
- Use fixed metric dimensions. Do not use business or request IDs as metric labels.
- Serve a valid warm snapshot during a Redis failure.
- Protect admin endpoints when `AD_ADMIN_TOKEN` has a value.

## Before a commit

1. Add a regression test for each behavior change.
2. Run `make verify`.
3. Run the Redis gate after a Redis, cache, lifecycle, or control-plane change.
4. Add a metric or health signal for a new failure path.
5. Update the related document after a contract change.

Run the Redis gate with this command:

```sh
INTEGRATION_REDIS_ADDR=127.0.0.1:6379 make verify-integration
```

Do not weaken a test, race check, fuzz check, coverage limit, alert, or CI job to make a change pass.

The integration gate requires at least 78% coverage in `internal/ads`.

Do not commit secrets, binaries, coverage files, or local data.
