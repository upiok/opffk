#!/bin/sh
# build.sh — build opffk release artifacts on a development host.
#
# Produces per-architecture release directories and tarballs:
#
#   dist/opffk-<version>-linux-<arch>/
#     install.sh                 the installer (copied verbatim)
#     S99opffk                   init script template
#     manifest.json              pinned upstream core manifest
#     VERSION                    opffk version marker
#     opffk-web-<arch>           the web backend (built here)
#     openflux-linux-<arch>      the upstream core (see --core below)
#   dist/opffk-<version>-linux-<arch>.tar.gz (+ .sha256)
#
# The upstream core binary is obtained by:
#   --core=download (default): download the pinned node-v* release from
#       upstream GitHub and verify its SHA-256 against manifest.json
#   --core=build: build from a local checkout of upstream
#       (path in $OPFFK_UPSTREAM or ../OpenFlux) — hashes will NOT match
#       the manifest, which is recorded in the produced manifest-dev.json
#   --core=none: skip the core (installer will fail later; CI/tests only)
#
# Architectures: amd64, arm64, arm (GOARM=7) — matching upstream releases.
# mipsle is buildable but experimental (see docs/COMPATIBILITY.md); it is
# produced only with --mipsle and marked experimental.

set -eu

cd "$(dirname "$0")/.."
ROOT_DIR=$(pwd)

VERSION=$(cat VERSION 2>/dev/null || echo "0.0.0-dev")
GO=${GO:-go}
UPSTREAM_DIR="${OPFFK_UPSTREAM:-$ROOT_DIR/../OpenFlux}"
CORE_MODE="download"
WITH_MIPSLE=0
OUT_DIR="$ROOT_DIR/dist"
TAGS=""
CORE_VERSION=""
CORE_SHA_FILE=""

usage() {
    grep '^#' "$0" | sed -n '2,30p' | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version) VERSION="$2"; shift 2 ;;
        --core) CORE_MODE="$2"; shift 2 ;;
        --upstream) UPSTREAM_DIR="$2"; shift 2 ;;
        --mipsle) WITH_MIPSLE=1; shift ;;
        --tags) TAGS="$2"; shift 2 ;;
        --out) OUT_DIR="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

command -v "$GO" >/dev/null 2>&1 || { echo "go toolchain not found (set GO=...)" >&2; exit 1; }

# read_core_manifest ARCH: extract sha256 from manifest.json via sed (no jq dependency).
read_core_manifest() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p" "$ROOT_DIR/manifest.json" | head -n 1
}
read_core_version() {
    sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ROOT_DIR/manifest.json" | head -n 1
}
read_core_url() {
    sed -n 's/.*"url_template"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ROOT_DIR/manifest.json" | head -n 1
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

build_backend() {
    _arch="$1" _out="$2"
    _goarch="$_arch"
    _goarm=""
    [ "$_arch" = "arm" ] && { _goarch="arm"; _goarm="7"; }
    echo "[build] opffk-web for linux/$_goarch${_goarm:+ (GOARM=$_goarm)}"
    ( cd "$ROOT_DIR/backend" && export CGO_ENABLED=0 GOOS=linux GOARCH="$_goarch"
      [ -n "$_goarm" ] && export GOARM="$_goarm"
      "$GO" build -buildvcs=false -trimpath -tags "$TAGS" \
        -ldflags "-s -w -X main.version=$VERSION" -o "$_out" . )
}

get_core_download() {
    _arch="$1" _dst="$2"
    _url=$(read_core_url | sed "s|{arch}|$_arch|g")
    _want=$(read_core_manifest "$_arch")
    [ -n "$_want" ] || { echo "manifest.json lacks sha256 for $_arch" >&2; return 1; }
    echo "[core] downloading $(basename "$_url")"
    curl -fsSL -o "$_dst" "$_url"
    _got=$(sha256_of "$_dst")
    if [ "$_got" != "$_want" ]; then
        echo "FATAL: core sha256 mismatch for $_arch" >&2
        echo "  expected: $_want" >&2
        echo "  got:      $_got" >&2
        return 1
    fi
    echo "[core] sha256 verified for $_arch"
}

get_core_build() {
    _arch="$1" _dst="$2"
    [ -d "$UPSTREAM_DIR" ] || { echo "upstream checkout not found: $UPSTREAM_DIR" >&2; return 1; }
    _goarch="$_arch"; _goarm=""
    [ "$_arch" = "arm" ] && _goarm="7"
    echo "[core] building upstream from $UPSTREAM_DIR (linux/$_goarch)"
    (cd "$UPSTREAM_DIR" && export CGO_ENABLED=0 GOOS=linux GOARCH="$_goarch"
     [ -n "$_goarm" ] && export GOARM="$_goarm"
     "$GO" build -buildvcs=false -trimpath -o "$_dst" .)
    CORE_VERSION=$(cd "$UPSTREAM_DIR" && git describe --tags --always 2>/dev/null || echo "source-build")
}

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

CORE_VERSION=$(read_core_version)

build_one() {
    _arch="$1" _label="$2" _experimental="$3"
    _rel="$OUT_DIR/opffk-$VERSION-linux-$_label"
    mkdir -p "$_rel"

    build_backend "$_arch" "$_rel/opffk-web-$_label"

    case "$CORE_MODE" in
        download) get_core_download "$_arch" "$_rel/openflux-linux-$_label" ;;
        build)    get_core_build "$_arch" "$_rel/openflux-linux-$_label" ;;
        none)     : ;;
        *) echo "bad --core mode: $CORE_MODE" >&2; exit 2 ;;
    esac

    cp install.sh "$_rel/install.sh"
    cp packaging/S99opffk "$_rel/S99opffk"
    cp manifest.json "$_rel/manifest.json"
    printf '%s\n' "$VERSION" > "$_rel/VERSION"
    if [ "$_experimental" = "1" ]; then
        printf '%s\n' "EXPERIMENTAL: $_label builds have no upstream release and no device testing. See docs/COMPATIBILITY.md." > "$_rel/EXPERIMENTAL"
    fi

    echo "[pkg] $(_rel_size "$_rel")"
    ( cd "$OUT_DIR" && tar -czf "opffk-$VERSION-linux-$_label.tar.gz" "opffk-$VERSION-linux-$_label" )
    _sum=$(sha256_of "$OUT_DIR/opffk-$VERSION-linux-$_label.tar.gz")
    printf '%s  opffk-%s-linux-%s.tar.gz\n' "$_sum" "$VERSION" "$_label" > "$OUT_DIR/opffk-$VERSION-linux-$_label.tar.gz.sha256"
}

_rel_size() {
    du -sh "$1" | cut -f1
}

build_one amd64 amd64 0
build_one arm64 arm64 0
build_one arm arm 0
[ "$WITH_MIPSLE" = "1" ] && build_one mipsle mipsle 1

echo
echo "release artifacts in $OUT_DIR:"
ls -1 "$OUT_DIR"
