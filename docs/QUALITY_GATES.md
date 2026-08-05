# Quality gates and monitoring

## Local gate

Run this command before each commit:

```sh
make verify
```

The command checks formatting, `go vet`, unit tests, race tests, fuzz smoke tests, coverage, and builds.

The local gate requires 65% coverage in `internal/ads`. The Redis gate uses the CI limit of 78%.

## Redis gate

Start an isolated Redis process. Then, run this command:

```sh
INTEGRATION_REDIS_ADDR=127.0.0.1:6379 make verify-integration
```

The integration tests use a unique key prefix. They remove their keys and do not flush the database.

These tests cover revision order, exact retries, delete tombstones, deterministic ordering, and Pub/Sub invalidations.

## Continuous integration

GitHub Actions starts Redis and runs the full verification script. The job has a 15-minute limit.

The workflow uploads the coverage profile after a successful run.

Protect `main` after you make the repository public:

1. Require a pull request.
2. Require one approval.
3. Require the `quality / verify` check.
4. Require the branch to be current before merge.
5. Block force pushes and branch deletion.

## Monitoring

Start the server before you start the monitoring services.

Set a local Grafana password:

```sh
export GRAFANA_ADMIN_PASSWORD='local-password'
make monitoring-up
```

The local services use these addresses:

| Service | Address |
| --- | --- |
| Grafana | `http://127.0.0.1:3000` |
| Prometheus | `http://127.0.0.1:9090` |
| Alertmanager | `http://127.0.0.1:9093` |

Prometheus reads `host.docker.internal:8080`. Change this target for a deployed environment.

The checked-in Alertmanager receiver does not send notifications. Configure and test the production receiver before deployment.

The dashboard shows auction volume, fill ratio, latency, snapshot failures, stale results, and control-plane errors.

It also shows DSP timeouts, failures, circuit state, refresh limits, and cache limits.

## Production checks

- Send alerts to the on-call system and test delivery.
- Set latency and error limits for the deployment.
- Add external probes for health, readiness, and a known auction.
- Test a managed Redis failover.
- Run a 30-minute load test from separate hardware.
