.PHONY: test test-integration verify verify-integration monitoring-up monitoring-down

test:
	go test -count=1 ./...

test-integration:
	test -n "$$INTEGRATION_REDIS_ADDR"
	INTEGRATION_REDIS_ADDR="$$INTEGRATION_REDIS_ADDR" go test -count=1 ./...

verify:
	./scripts/verify.sh

verify-integration:
	test -n "$$INTEGRATION_REDIS_ADDR"
	COVERAGE_MIN=78 INTEGRATION_REDIS_ADDR="$$INTEGRATION_REDIS_ADDR" ./scripts/verify.sh

monitoring-up:
	test -n "$$GRAFANA_ADMIN_PASSWORD"
	docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d

monitoring-down:
	docker compose -f docker-compose.yml -f docker-compose.monitoring.yml down
