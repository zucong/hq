#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
if command -v hq >/dev/null 2>&1; then
  echo "请在未安装 hq 的干净 shell 运行 README 首验" >&2
  exit 64
fi

temp_base=${TMPDIR:-/tmp}
temp_base=$(CDPATH= cd -- "$temp_base" && pwd -P)
smoke_root=$(mktemp -d "$temp_base/hq-first-run.XXXXXX")
cleanup_readme_smoke() {
  if [ -n "${smoke_root:-}" ] && [ -d "$smoke_root" ]; then rm -rf -- "$smoke_root"; fi
}
trap cleanup_readme_smoke EXIT HUP INT TERM
mkdir -p "$smoke_root/hq/cmd/hq" "$smoke_root/tmp" "$smoke_root/go-cache" "$smoke_root/home" "$smoke_root/empty-path"
cp "$source_dir"/cmd/hq/*.go "$smoke_root/hq/cmd/hq/"
cp "$source_dir/go.mod" "$source_dir/go.sum" "$smoke_root/hq/"
cd "$smoke_root/hq"
TMPDIR="$smoke_root/tmp" GOCACHE="$smoke_root/go-cache" go build -trimpath -o ./bin/hq ./cmd/hq
./bin/hq help >/dev/null
run_clean() {
  env -i PATH="$smoke_root/empty-path" HOME="$smoke_root/home" TMPDIR="$smoke_root/tmp" "$@"
}
if run_clean /bin/sh -c 'command -v herdr >/dev/null 2>&1'; then
  echo "clean PATH unexpectedly contains herdr" >&2
  exit 1
fi
run_clean ./bin/hq init "$smoke_root/headquarters" --silent \
  --company-name "Smoke Company" --owner ZC \
  --workspace smoke-company-hq --template minimal --prepare-only
if [ ! -f "$smoke_root/headquarters/ceo-office/tools/hq/config.yaml" ]; then
  echo "init did not create the canonical organization registry" >&2
  exit 1
fi
if find "$smoke_root/headquarters" -name 'ROSTER.md' -print | grep -q .; then
  echo "init created a forbidden parallel Markdown organization registry" >&2
  exit 1
fi
run_clean ./bin/hq --office "$smoke_root/headquarters/ceo-office" staff list
run_clean ./bin/hq --office "$smoke_root/headquarters/ceo-office" board --cases-only
for path in \
  "$smoke_root/headquarters/ceo-office/records/events" \
  "$smoke_root/headquarters/ceo-office/records/.hq.lock" \
  "$smoke_root/headquarters/ceo-office/records/state.json" \
  "$smoke_root/headquarters/ceo-office/records/hq.sock" \
  "$smoke_root/headquarters/ceo-office/tools/index.db"
do
  if [ -e "$path" ]; then
    echo "read-only first use created $path" >&2
    exit 1
  fi
done
echo "README_SMOKE PASS"
