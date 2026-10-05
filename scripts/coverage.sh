#!/bin/sh
# Coverage gate, in the spirit of szmy's "coverage must not regress" rule.
# Fails when total statement coverage of ./internal/... is below COVER_MIN (default 85).
set -eu

MIN="${COVER_MIN:-85}"
go test -covermode=atomic -coverprofile=coverage.out ./internal/...

total=$(go tool cover -func=coverage.out | awk '/^total:/ { gsub("%", "", $3); print $3 }')
echo "total coverage: ${total}% (minimum ${MIN}%)"
awk -v t="$total" -v m="$MIN" 'BEGIN { exit (t + 0 >= m + 0) ? 0 : 1 }' || {
  echo "coverage gate failed" >&2
  exit 1
}
