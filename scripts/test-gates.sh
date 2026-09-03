#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
temp_base=${TMPDIR:-/tmp}
temp_base=$(CDPATH= cd -- "$temp_base" && pwd -P)
gate_root=$(mktemp -d "$temp_base/hq-gates.XXXXXX")
cleanup_gates() {
  if [ -n "${gate_root:-}" ] && [ -d "$gate_root" ]; then rm -rf -- "$gate_root"; fi
}
trap cleanup_gates EXIT HUP INT TERM
mkdir -p "$gate_root/tmp" "$gate_root/go-cache"
export TMPDIR="$gate_root/tmp"
export GOCACHE="$gate_root/go-cache"
cd "$source_dir"

echo "GATE 01 shell-syntax"
sh -n ./scripts/*.sh

round=1
while [ "$round" -le 3 ]; do
  echo "GATE 02.$round cli-release-directed"
  go test -count=1 -run '^TestCLIRelease' ./...
  round=$((round + 1))
done

round=1
while [ "$round" -le 3 ]; do
  echo "GATE 02D.$round delivery-policy-directed"
  go test -count=1 -run '^TestDeliveryPolicy' ./...
  round=$((round + 1))
done

echo "GATE 03 acceptance"
./scripts/test-acceptance.sh

echo "GATE 04 readme-smoke"
./scripts/smoke-readme.sh

echo "GATE 05 full"
go test -count=1 ./...

echo "GATE 06 race"
go test -race ./...

echo "GATE 07 vet"
go vet ./...

echo "GATE 08 gofmt"
gofmt -d ./cmd/hq/*.go > "$gate_root/gofmt.diff"
if [ -s "$gate_root/gofmt.diff" ]; then
  cat "$gate_root/gofmt.diff" >&2
  exit 1
fi

echo "GATE 09 test-enumeration"
listed=$(go test -list '^Test' ./... | awk '/^Test/{count++} END{print count+0}')
source_count=$(awk '/^func Test/{count++} END{print count+0}' ./cmd/hq/*_test.go)
if [ "$listed" -ne "$source_count" ]; then
  echo "test enumeration mismatch: listed=$listed source=$source_count" >&2
  exit 1
fi
echo "test_count=$listed"

echo "GATE 10 current-build"
go build -trimpath -o "$gate_root/hq" ./cmd/hq
"$gate_root/hq" version --json

echo "GATE 11 release-build"
head_commit=$(git rev-parse HEAD)
HQ_RELEASE_REHEARSAL=1 ./scripts/release.sh build v0.0.0 "$head_commit" "$gate_root/release"
./scripts/release.sh verify "$gate_root/release"
host_goos=$(go env GOOS)
host_goarch=$(go env GOARCH)
"$gate_root/release/hq_v0.0.0_${host_goos}_${host_goarch}" version --json

echo "ALL_GATES PASS"
