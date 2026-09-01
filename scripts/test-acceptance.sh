#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
temp_base=${TMPDIR:-/tmp}
temp_base=$(CDPATH= cd -- "$temp_base" && pwd -P)
test_root=$(mktemp -d "$temp_base/hq-acceptance.XXXXXX")
cleanup_acceptance() {
  if [ -n "${test_root:-}" ] && [ -d "$test_root" ]; then rm -rf -- "$test_root"; fi
}
trap cleanup_acceptance EXIT HUP INT TERM
mkdir -p "$test_root/tmp" "$test_root/go-cache"

cd "$source_dir"
TMPDIR="$test_root/tmp" GOCACHE="$test_root/go-cache" \
  go test -count=1 -run '^TestCLIReleaseAcceptance$' -v ./...
