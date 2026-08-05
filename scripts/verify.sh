#!/bin/sh
set -eu

project_tmp="${TMPDIR:-/tmp}/adserver-verification"
mkdir -p "$project_tmp/go-cache"
export GOCACHE="$project_tmp/go-cache"

unformatted="$(gofmt -l ./cmd ./internal)"
if [ -n "$unformatted" ]; then
	echo "gofmt required:"
	echo "$unformatted"
	exit 1
fi

go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

for target in FuzzBidValidationNeverPanics FuzzAuctionRequestValidationNeverPanics; do
	go test ./internal/ads -run='^$' -fuzz="^${target}$" -fuzztime=2s
done

coverage_file="$project_tmp/coverage.out"
go test -count=1 -coverprofile="$coverage_file" ./internal/ads
coverage="$(go tool cover -func="$coverage_file" | awk '/^total:/ {gsub("%", "", $3); print $3}')"
minimum="${COVERAGE_MIN:-65}"
awk -v actual="$coverage" -v required="$minimum" 'BEGIN {
	if (actual + 0 < required + 0) {
		printf "coverage %.1f%% is below required %.1f%%\n", actual, required
		exit 1
	}
	printf "coverage %.1f%% meets required %.1f%%\n", actual, required
}'

go build ./...
