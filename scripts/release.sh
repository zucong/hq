#!/bin/sh
set -eu

usage() {
  echo "用法：" >&2
  echo "  $0 build VERSION COMMIT OUTPUT_DIR" >&2
  echo "  $0 verify OUTPUT_DIR" >&2
  exit 64
}

checksum_tool() {
  if command -v sha256sum >/dev/null 2>&1; then
    echo sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    echo shasum
  else
    echo "缺少 sha256sum 或 shasum" >&2
    exit 69
  fi
}

verify_release() {
  release_dir=$1
  [ -d "$release_dir" ] || { echo "release 目录不存在：$release_dir" >&2; exit 66; }
  [ -f "$release_dir/SHA256SUMS" ] || { echo "缺少 SHA256SUMS：$release_dir" >&2; exit 66; }
  tool=$(checksum_tool)
  if [ "$tool" = sha256sum ]; then
    (cd "$release_dir" && sha256sum -c SHA256SUMS)
  else
    (cd "$release_dir" && shasum -a 256 -c SHA256SUMS)
  fi
}

[ "$#" -ge 1 ] || usage
action=$1
shift

if [ "$action" = verify ]; then
  [ "$#" -eq 1 ] || usage
  verify_release "$1"
  exit 0
fi

[ "$action" = build ] || usage
[ "$#" -eq 3 ] || usage
version=$1
commit=$2
output_dir=$3

if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "VERSION 必须是无前导零的正式语义版本，例如 v1.2.3：$version" >&2
  exit 64
fi
case "$commit" in
  *[!0-9a-f]*|??????|?????|????|???|??|?|"" ) echo "COMMIT 必须是至少 7 位小写十六进制：$commit" >&2; exit 64 ;;
esac
[ ! -e "$output_dir" ] || { echo "OUTPUT_DIR 已存在，拒绝覆盖：$output_dir" >&2; exit 73; }

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
if [ "${HQ_RELEASE_REHEARSAL:-0}" != 1 ]; then
  head=$(git -C "$source_dir" rev-parse HEAD)
  [ "$head" = "$commit" ] || { echo "COMMIT 与 HEAD 不一致：$commit != $head" >&2; exit 75; }
  [ -z "$(git -C "$source_dir" status --porcelain -- .)" ] || { echo "HQ 源码不干净，拒绝正式 release build" >&2; exit 75; }
fi

output_parent=$(dirname -- "$output_dir")
mkdir -p "$output_parent"
output_parent=$(CDPATH= cd -- "$output_parent" && pwd -P)
output_name=$(basename -- "$output_dir")
stage=$(mktemp -d "$output_parent/.hq-release.XXXXXX")
cleanup_release_stage() {
  if [ -n "${stage:-}" ] && [ -d "$stage" ]; then rm -rf -- "$stage"; fi
}
trap cleanup_release_stage EXIT HUP INT TERM

go_version=$(go version)
ldflags="-X main.buildVersion=$version -X main.buildCommit=$commit"
for target in darwin/arm64 linux/amd64 linux/arm64; do
  goos=${target%/*}
  goarch=${target#*/}
  name="hq_${version}_${goos}_${goarch}"
  (cd "$source_dir" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$ldflags" -o "$stage/$name" .)
done

{
  echo "version=$version"
  echo "commit=$commit"
  echo "go=$go_version"
  echo "targets=darwin/arm64,linux/amd64,linux/arm64"
  echo "trimpath=true"
  echo "wall_clock_embedded=false"
} > "$stage/BUILD-MANIFEST.txt"

tool=$(checksum_tool)
if [ "$tool" = sha256sum ]; then
  (cd "$stage" && sha256sum hq_* BUILD-MANIFEST.txt > SHA256SUMS)
else
  (cd "$stage" && shasum -a 256 hq_* BUILD-MANIFEST.txt > SHA256SUMS)
fi
verify_release "$stage"
mv "$stage" "$output_parent/$output_name"
stage=""
trap - EXIT HUP INT TERM
echo "release=$output_parent/$output_name"
