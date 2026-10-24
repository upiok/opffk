#!/bin/sh
# installer_test.sh — unit tests for install.sh helper functions.
# Sources install.sh with OPFFK_SOURCE_ONLY=1 and exercises the pure
# helpers plus the sandboxed install/uninstall flow end to end.
#
# Usage: sh tests/installer_test.sh

set -u

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
PASS=0
FAIL=0

ok()   { PASS=$((PASS+1)); printf '[sh ok] %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '[sh FAIL] %s\n' "$1" >&2; }

expect_eq() { # desc want got
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: want '$2' got '$3'"; fi
}
expect_rc() { # desc want_rc cmd...
    _want="$2"; shift 2
    "$@" >/dev/null 2>&1
    _rc=$?
    if [ "$_rc" = "$_want" ]; then ok "$1 $2"; else bad "$*: rc=$_rc want=$_want"; fi
}

# ---- load the installer as a library -----------------------------------------

OPFFK_SOURCE_ONLY=1
export OPFFK_SOURCE_ONLY
# shellcheck disable=SC1091
. "$ROOT_DIR/install.sh"
unset OPFFK_SOURCE_ONLY

# silence the installer's own output in assertions
exec 9>/dev/null

# ---- pure helpers -------------------------------------------------------------

expect_eq "detect_arch x86_64"  "amd64" "$(detect_arch x86_64)"
expect_eq "detect_arch aarch64" "arm64" "$(detect_arch aarch64)"
expect_eq "detect_arch armv7l"  "arm"   "$(detect_arch armv7l)"
expect_eq "detect_arch armv7hl" "arm"   "$(detect_arch armv7hl)"
case "$(detect_arch mips)" in
    unsupported:*) ok "detect_arch mips unsupported" ;;
    *) bad "detect_arch mips should be unsupported" ;;
esac
case "$(detect_arch armv6l)" in
    unsupported:*) ok "detect_arch armv6l unsupported" ;;
    *) bad "detect_arch armv6l should be unsupported" ;;
esac

expect_eq "validate_port 8791"   "0" "$(validate_port 8791; echo $?)"
expect_eq "validate_port 1"      "0" "$(validate_port 1; echo $?)"
expect_eq "validate_port 65535"  "0" "$(validate_port 65535; echo $?)"
expect_eq "validate_port 65536"  "1" "$(validate_port 65536; echo $?)"
expect_eq "validate_port 0"      "1" "$(validate_port 0; echo $?)"
expect_eq "validate_port abc"    "1" "$(validate_port abc; echo $?)"
expect_eq "validate_port empty"  "1" "$(validate_port ''; echo $?)"

expect_eq "validate_ipv4 ok"     "0" "$(validate_ipv4 192.168.1.1; echo $?)"
expect_eq "validate_ipv4 zero"   "0" "$(validate_ipv4 0.0.0.0; echo $?)"
expect_eq "validate_ipv4 oct255" "0" "$(validate_ipv4 10.255.255.255; echo $?)"
expect_eq "validate_ipv4 256"    "1" "$(validate_ipv4 256.1.1.1; echo $?)"
expect_eq "validate_ipv4 short"  "1" "$(validate_ipv4 10.0.1; echo $?)"
expect_eq "validate_ipv4 empty"  "1" "$(validate_ipv4 ''; echo $?)"
expect_eq "validate_ipv4 text"   "1" "$(validate_ipv4 a.b.c.d; echo $?)"

expect_eq "validate_key_hex ok"   "0" "$(validate_key_hex 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef; echo $?)"
expect_eq "validate_key_hex short" "1" "$(validate_key_hex abcdef; echo $?)"
expect_eq "validate_key_hex nonhex" "1" "$(validate_key_hex zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz; echo $?)"
expect_eq "validate_key_hex empty" "1" "$(validate_key_hex ''; echo $?)"

# mem/disk helpers run on the dev host; just check they produce numbers.
case "$(mem_available_kb)" in
    ''|*[!0-9]*) bad "mem_available_kb not numeric: $(mem_available_kb)" ;;
    *) ok "mem_available_kb numeric" ;;
esac
case "$(disk_free_kb /tmp)" in
    ''|*[!0-9]*) bad "disk_free_kb not numeric: $(disk_free_kb /tmp)" ;;
    *) ok "disk_free_kb numeric" ;;
esac

# port_is_busy: pick a port we know is closed and one we can open.
expect_eq "port_is_busy closed port" "1" "$(port_is_busy 18699; echo $?)"
if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket,time,sys; s=socket.socket(); s.bind(("127.0.0.1",18698)); s.listen(1); time.sleep(3)' &
    PYPID=$!
    sleep 0.4
    expect_eq "port_is_busy listening port" "0" "$(port_is_busy 18698; echo $?)"
    kill $PYPID 2>/dev/null
    wait $PYPID 2>/dev/null
else
    ok "port_is_busy listening port (skipped, no python3)"
fi

# sha256 helpers
_tmpf=$(mktemp)
printf 'opffk-test-content' > "$_tmpf"
_want=$( (command -v sha256sum >/dev/null && sha256sum "$_tmpf") || shasum -a 256 "$_tmpf" | awk '{print $1}')
_want=${_want%% *}
expect_eq "sha256_of known content" "$_want" "$(sha256_of "$_tmpf")"
expect_eq "verify_sha256 good" "0" "$(verify_sha256 "$_tmpf" "$_want"; echo $?)"
expect_eq "verify_sha256 bad"  "1" "$(verify_sha256 "$_tmpf" 0000000000000000000000000000000000000000000000000000000000000000; echo $?)"
rm -f "$_tmpf"

# ---- regression: the curl|sh download path (stage_from_release) ---------------
# Reproduces the exact flow of the one-command install without network:
# a nested-layout tarball (what build.sh produces) is served to the
# installer through the OPFFK_FETCH_CMD test hook, and the installer must
# flatten it, verify hashes and complete the install.
HOST_ARCH=$(detect_arch "$(uname -m)")
WORK2=$(mktemp -d "${TMPDIR:-/tmp}/opffk-fetchtest.XXXXXX")
REL2="$WORK2/rel"; SBX2="$WORK2/sbx"
mkdir -p "$REL2" "$SBX2"
cp "$ROOT_DIR/install.sh" "$REL2/install.sh"
cp "$ROOT_DIR/packaging/S99opffk" "$REL2/S99opffk"
cp "$ROOT_DIR/manifest.json" "$REL2/manifest.json"
printf '#!/bin/sh\necho fake\n' > "$REL2/opffk-web-$HOST_ARCH"
printf '#!/bin/sh\nsleep 30\n' > "$REL2/openflux-linux-$HOST_ARCH"
chmod +x "$REL2/opffk-web-$HOST_ARCH" "$REL2/openflux-linux-$HOST_ARCH"
FAKE_SHA2=$(sha256_of "$REL2/openflux-linux-$HOST_ARCH")
sed "s/\"$HOST_ARCH\": \"[0-9a-f]\{64\}\"/\"$HOST_ARCH\": \"$FAKE_SHA2\"/; s/\"version\": \"node-v[^\"]*\"/\"version\": \"node-v0.0.0-test\"/" \
    "$ROOT_DIR/manifest.json" > "$REL2/manifest.json"
printf 'node-v0.0.0-test\n' > "$REL2/VERSION"
# nested-layout tarball, exactly like scripts/build.sh packages it.
# Keep a pristine copy of install.sh outside the release dir: the piped
# curl|sh flow runs the freshly downloaded script, not the one in the
# tarball.
cp "$REL2/install.sh" "$WORK2/install.sh"
mkdir "$REL2/opffk-v9.9.9-linux-amd64"
mv "$REL2/install.sh" "$REL2/S99opffk" "$REL2/manifest.json" "$REL2/VERSION" \
   "$REL2/opffk-web-$HOST_ARCH" "$REL2/openflux-linux-$HOST_ARCH" \
   "$REL2/opffk-v9.9.9-linux-amd64/"
( cd "$REL2" && tar -czf "$WORK2/opffk-v9.9.9-linux-amd64.tar.gz" "opffk-v9.9.9-linux-amd64" )
cat > "$WORK2/fetch-hook.sh" <<'HOOK'
#!/bin/sh
case "$1" in
    *.sha256) sha256sum "$FETCH_TARBALL" > "$2" ;;
    *) cp "$FETCH_TARBALL" "$2" ;;
esac
HOOK
chmod +x "$WORK2/fetch-hook.sh"
OUT2=$(OPFFK_TEST_MODE=1 OPFFK_FETCH_CMD="$WORK2/fetch-hook.sh" FETCH_TARBALL="$WORK2/opffk-v9.9.9-linux-amd64.tar.gz" \
    sh "$WORK2/install.sh" install --repo test/test --version v9.9.9 \
    --prefix "$SBX2" --no-start 2>&1)
if [ "$?" != "0" ]; then printf '%s\n' "$OUT2" | sed -n '1,20p' >&2; fi
expect_eq "curl|sh path: nested tarball install succeeds" "0" "$?"
[ -x "$SBX2/etc/init.d/S99opffk" ] && ok "curl|sh path: init script installed" || bad "curl|sh path: init script missing"
[ -x "$SBX2/bin/openflux" ] && ok "curl|sh path: core binary installed" || bad "curl|sh path: core binary missing"
case "$OUT2" in
    *"release unpacked to staging"*) ok "curl|sh path: tarball flattened" ;;
    *) bad "curl|sh path: flatten step not reached"; printf '%s\n' "$OUT2" | sed -n '1,15p' >&2 ;;
esac
rm -rf "$WORK2"

# ---- sandboxed install flow (test mode) ---------------------------------------

WORK=$(mktemp -d "${TMPDIR:-/tmp}/opffk-insttest.XXXXXX")
REL="$WORK/rel"; SBX="$WORK/sbx"
mkdir -p "$REL" "$SBX"

# Minimal release directory (no core binary → installer must refuse).
cp "$ROOT_DIR/install.sh" "$REL/install.sh"
cp "$ROOT_DIR/packaging/S99opffk" "$REL/S99opffk"
cp "$ROOT_DIR/manifest.json" "$REL/manifest.json"

HOST_ARCH=$(detect_arch "$(uname -m)")
case "$HOST_ARCH" in amd64|arm64|arm) : ;; *) echo "host arch not testable: $HOST_ARCH"; exit 0 ;; esac

# without any opffk-web binary the installer must fail loudly
OPFFK_TEST_MODE=1 sh "$ROOT_DIR/install.sh" install --from-dir "$REL" --prefix "$SBX" \
    --listen-ip 127.0.0.1 --port 18797 >/dev/null 2>&1
expect_eq "install without backend binary fails" "1" "$?"

printf '#!/bin/sh\necho fake\n' > "$REL/opffk-web-$HOST_ARCH"
chmod +x "$REL/opffk-web-$HOST_ARCH"
# a fake core too; the manifest is rewritten to pin its actual hash the
# same way build.sh pins the real upstream binary's hash
printf '#!/bin/sh\nsleep 30\n' > "$REL/openflux-linux-$HOST_ARCH"
chmod +x "$REL/openflux-linux-$HOST_ARCH"
printf 'node-v0.0.0-test\n' > "$REL/VERSION"
FAKE_SHA=$(sha256_of "$REL/openflux-linux-$HOST_ARCH")
sed "s/\"$HOST_ARCH\": \"[0-9a-f]\{64\}\"/\"$HOST_ARCH\": \"$FAKE_SHA\"/; s/\"version\": \"node-v[^\"]*\"/\"version\": \"node-v0.0.0-test\"/" \
    "$ROOT_DIR/manifest.json" > "$REL/manifest.json"

# --no-start: this test's fake backend cannot serve HTTP; the real
# start+credential flow is covered by tests/e2e_smoke.sh.
OUT=$(OPFFK_TEST_MODE=1 sh "$ROOT_DIR/install.sh" install --from-dir "$REL" --prefix "$SBX" \
    --listen-ip 127.0.0.1 --port 18797 --no-start 2>&1)
RC=$?
expect_eq "sandbox install succeeds" "0" "$RC"
[ -x "$SBX/etc/init.d/S99opffk" ] && ok "init script installed executable" || bad "init script missing"
[ -f "$SBX/share/opffk/manifest.json" ] && ok "manifest installed" || bad "manifest missing"
grep -q "OPFFK_WEB_ARGS=\"-listen 127.0.0.1 -port 18797\"" "$SBX/etc/init.d/S99opffk" \
    && ok "listen/port baked into init script" || bad "init script flags not baked"
# (no ui.json yet: the backend generates it and the one-time password on
# its first real start — covered by the e2e suite)

# idempotency: a second install (upgrade path) succeeds and backs up
OPFFK_TEST_MODE=1 sh "$ROOT_DIR/install.sh" install --from-dir "$REL" --prefix "$SBX" \
    --listen-ip 127.0.0.1 --port 18797 --no-start >/dev/null 2>&1
expect_eq "second install succeeds" "0" "$?"
ls "$SBX"/var/lib/opffk/opffk-backup-*.tar.gz >/dev/null 2>&1 && ok "upgrade created a backup" || bad "no backup on upgrade"

# init-script lifecycle with a fake long-running backend: exercises the
# start/stop mechanics (setsid/plain-background fallback, pidfile) under
# whatever busybox applets the platform has — nohup is absent on Keenetic.
printf '#!/bin/sh\nsleep 60\n' > "$SBX/bin/opffk-web"
chmod +x "$SBX/bin/opffk-web"
rm -f "$SBX/etc/opffk/ui.json" 2>/dev/null
if "$SBX/etc/init.d/S99opffk" start >/dev/null 2>&1 && "$SBX/etc/init.d/S99opffk" status >/dev/null 2>&1; then
    ok "init script: start + status work without nohup"
else
    bad "init script: start/status failed (nohup or pidfile problem)"
fi
if "$SBX/etc/init.d/S99opffk" stop >/dev/null 2>&1 && ! "$SBX/etc/init.d/S99opffk" status >/dev/null 2>&1; then
    ok "init script: stop works"
else
    bad "init script: stop failed"
fi
# restore the failing fake so later uninstall assertions stay valid
printf '#!/bin/sh\necho fake\n' > "$SBX/bin/opffk-web"
chmod +x "$SBX/bin/opffk-web"

# status command
OUT=$(OPFFK_TEST_MODE=1 sh "$ROOT_DIR/install.sh" --prefix "$SBX" status)
case "$OUT" in *"installed"*) ok "status reports installed" ;; *) bad "status output wrong: $OUT" ;; esac

# uninstall
OPFFK_TEST_MODE=1 sh "$ROOT_DIR/install.sh" uninstall --prefix "$SBX" >/dev/null 2>&1
expect_eq "uninstall succeeds" "0" "$?"
[ -x "$SBX/bin/opffk-web" ] && bad "backend binary survived uninstall" || ok "backend binary removed"
[ -d "$SBX/etc/opffk" ] && bad "config dir survived uninstall" || ok "config dir removed"
ls "$SBX"/var/lib/opffk/opffk-backup-*.tar.gz >/dev/null 2>&1 && ok "uninstall kept a backup" || bad "uninstall backup missing"

rm -rf "$WORK"

echo
if [ "$FAIL" -eq 0 ]; then
    echo "[sh tests] ALL $PASS CHECKS PASSED"
else
    echo "[sh tests] $PASS passed, $FAIL FAILED" >&2
    exit 1
fi
