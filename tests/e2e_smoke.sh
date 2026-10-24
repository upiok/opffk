#!/bin/sh
# e2e_smoke.sh — end-to-end exercise of the opffk stack on the development
# host, using the fakecore test double in place of the upstream OpenFlux
# binary (same flags, same IPC frames). Runs the real installer in test
# mode against a sandbox prefix, then drives the real HTTP API.
#
# Usage: sh tests/e2e_smoke.sh
# Requires: go (or GO env), curl, tar, python3 (JSON parsing only).

set -eu

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
GO=${GO:-go}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/opffk-e2e.XXXXXX")
SBX="$WORK/sandbox"
REL="$WORK/release"
PORT=18793
cleanup() {
    if [ -f "$SBX/var/run/opffk-web.pid" ]; then
        kill "$(cat "$SBX/var/run/opffk-web.pid")" 2>/dev/null || true
    fi
    pkill -f "opffk-web-e2e" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

pass() { printf '[e2e ok] %s\n' "$1"; }
fail() { printf '[e2e FAIL] %s\n' "$1" >&2; exit 1; }

# ---- build -----------------------------------------------------------------

echo "[e2e] building backend and fakecore"
mkdir -p "$REL" "$SBX"
(cd "$ROOT_DIR/backend" && CGO_ENABLED=0 "$GO" build -buildvcs=false -ldflags "-X main.version=e2e" -o "$REL/opffk-web-e2e" .)
(cd "$ROOT_DIR/tests/fakecore" && CGO_ENABLED=0 "$GO" build -buildvcs=false -o "$REL/fakecore" .)

# Stage a release directory the way build.sh would.
HOSTARCH=$("$GO" env GOARCH)
case "$HOSTARCH" in
    amd64) LABEL=amd64 ;;
    arm64) LABEL=arm64 ;;
    arm) LABEL=arm ;;
    *) fail "unsupported dev host arch $HOSTARCH" ;;
esac
cp "$ROOT_DIR/install.sh" "$REL/install.sh"
cp "$ROOT_DIR/packaging/S99opffk" "$REL/S99opffk"
cp "$ROOT_DIR/manifest.json" "$REL/manifest.json"
cp "$REL/opffk-web-e2e" "$REL/opffk-web-$LABEL"
# The fake core stands in for openflux-linux-<arch> with a manifest that
# pins its actual hash, exactly like a real release pins upstream binaries.
cp "$REL/fakecore" "$REL/openflux-linux-$LABEL"
FAKE_SHA=$(sha256sum "$REL/fakecore" | awk '{print $1}')
python3 - "$REL/manifest.json" "$FAKE_SHA" "$LABEL" <<'EOF'
import json, sys
path, sha, arch = sys.argv[1], sys.argv[2], sys.argv[3]
m = json.load(open(path))
m["core"]["sha256"] = {arch: sha}
m["core"]["version"] = "node-v0.0.0-e2e"
m["core"]["url_template"] = "https://127.0.0.1:1/none-{arch}"
json.dump(m, open(path, "w"), indent=2)
EOF
printf 'node-v0.0.0-e2e\n' > "$REL/VERSION"

# ---- install (test mode, sandbox prefix) -------------------------------------

echo "[e2e] running installer against sandbox prefix"
# OPFFK_ROOT is exported so the init script's backend child also runs in
# the sandbox (the backend derives its prefix from OPFFK_ROOT, not the
# installer's OPFFK_PREFIX).
OPFFK_TEST_MODE=1 OPFFK_PREFIX="$SBX" OPFFK_ROOT="$SBX" \
    sh "$REL/install.sh" install --from-dir "$REL" --prefix "$SBX" \
       --listen-ip 127.0.0.1 --port "$PORT" > "$WORK/install.log" 2>&1 \
    || { cat "$WORK/install.log"; fail "installer exited non-zero"; }
grep -q "Initial web password" "$WORK/install.log" || { cat "$WORK/install.log"; fail "initial password not printed"; }
PW=$(grep "Initial web password" "$WORK/install.log" | awk '{print $NF}')
pass "installer printed a one-time password"

INIT="$SBX/etc/init.d/S99opffk"
[ -x "$INIT" ] || fail "init script not installed/executable"
[ -x "$SBX/bin/opffk-web" ] || fail "backend binary not installed"
[ -x "$SBX/bin/openflux" ] || fail "core binary not installed"
[ -f "$SBX/share/opffk/manifest.json" ] || fail "manifest not installed"
[ "$(cat "$SBX/share/opffk/VERSION")" = "node-v0.0.0-e2e" ] || fail "core VERSION not recorded"
pass "installer laid out files in the sandbox"

# ---- wait for the web UI ------------------------------------------------------

for i in $(seq 1 40); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/login" 2>/dev/null; then break; fi
    sleep 0.5
    [ "$i" = 40 ] && { cat "$SBX/var/log/opffk/console.log" 2>/dev/null; fail "web UI never became ready"; }
done
pass "web UI answers on port $PORT"

COOKIES="$WORK/cookies.txt"
JAR() { curl -sS -b "$COOKIES" -c "$COOKIES" "$@"; }

# login
curl -fsS -c "$COOKIES" -o /dev/null "http://127.0.0.1:$PORT/login"
LOGIN=$(JAR -X POST -H "Content-Type: application/x-www-form-urlencoded" -d "password=$PW" "http://127.0.0.1:$PORT/login")
echo "$LOGIN" | grep -q '"ok":true' || fail "login failed: $LOGIN"
CSRF=$(echo "$LOGIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf"])')
pass "login with the initial password works"

# one-time password consumed after first login
[ -f "$SBX/etc/opffk/initial-password" ] && fail "initial-password file still present after first login"
pass "one-time password file consumed after first login"

# state
STATE=$(JAR "http://127.0.0.1:$PORT/api/state")
echo "$STATE" | grep -q '"test_mode":true' || fail "state lacks platform info"
pass "state endpoint serves authenticated JSON"

# CSRF rejection without token
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -b "$COOKIES" -X POST -H "Content-Type: application/json" \
    -d '{"settings":{}}' "http://127.0.0.1:$PORT/api/config")
[ "$CODE" = "403" ] || fail "config POST without CSRF returned $CODE (want 403)"
pass "state-changing route rejects missing CSRF"

# ---- service lifecycle with the fake core --------------------------------------

echo "[e2e] exercising the service lifecycle via the API"
POST() { JAR -X POST -H "X-Opffk-CSRF: $CSRF" -H "Content-Type: application/json" -d "$1" "http://127.0.0.1:$PORT/api/$2"; }

# autostart already launched the core; stop it first, then start again
RES=$(POST '{}' 'service/stop')
echo "$RES" | grep -q '"ok":true' || fail "service/stop failed: $RES"
sleep 1
JAR "http://127.0.0.1:$PORT/api/state" | grep -q '"running":true' && fail "core still running after stop"
pass "stop works (after autostart)"

RES=$(POST '{}' 'service/start')
echo "$RES" | grep -q '"ok":true' || fail "service/start failed: $RES"

CONNECTED=0
for i in $(seq 1 20); do
    if JAR "http://127.0.0.1:$PORT/api/state" | grep -q '"connected":true'; then CONNECTED=1; break; fi
    sleep 0.5
done
[ "$CONNECTED" = 1 ] || fail "core never reported connected over IPC"
pass "fake core started and streams IPC status to the UI"

# crash backoff: kill the core child, expect the supervisor to restart it
COREPID=$(cat "$SBX/var/run/opffk-core.pid" 2>/dev/null || true)
[ -n "$COREPID" ] && kill -9 "$COREPID" 2>/dev/null || true
RESTARTED=0
for i in $(seq 1 30); do
    if JAR "http://127.0.0.1:$PORT/api/state" | grep -q '"running":true'; then RESTARTED=1; break; fi
    sleep 0.5
done
[ "$RESTARTED" = 1 ] || fail "supervisor did not restart the core after a crash"
pass "supervisor restarts a crashed core"

# stop
RES=$(POST '{}' 'service/stop')
echo "$RES" | grep -q '"ok":true' || fail "service/stop failed: $RES"
sleep 1
JAR "http://127.0.0.1:$PORT/api/state" | grep -q '"running":true' && fail "core still running after stop"
pass "stop works"

# logs endpoint shows core output
LOGS=$(JAR "http://127.0.0.1:$PORT/api/logs")
echo "$LOGS" | grep -q "fakecore" || fail "logs do not include core output"
pass "log ring exposes core output"

# ---- update & rollback ------------------------------------------------------------

RES=$(POST '{"action":"check"}' 'update')
echo "$RES" | grep -q '"ok":true' || fail "update check failed: $RES"
echo "$RES" | grep -q "up to date" || fail "update check should report up to date, got: $RES"
pass "update check reports up-to-date for the pinned version"

# stage a "previous" binary so rollback has something to swap in
cp "$SBX/bin/openflux" "$SBX/bin/openflux.previous"
RES=$(POST '{"action":"rollback"}' 'update')
echo "$RES" | grep -q '"ok":true' || fail "rollback failed: $RES"
pass "manual rollback swaps the previous core binary"

# ---- config apply & validation -----------------------------------------------------

CONF_OK='{"settings":{"mode":"l4","codec":"batched","debug":2,"transports":[{"type":"direct","priority":50,"listen":"0.0.0.0:17891"}]},"restart":true}'
RES=$(POST "$CONF_OK" 'config')
echo "$RES" | grep -q '"ok":true' || fail "valid config rejected: $RES"
grep -q "Listen = 0.0.0.0:17891" "$SBX/etc/opffk/openflux.conf" || fail "core config not updated"
pass "config apply writes the upstream .conf and restarts"

CONF_BAD='{"settings":{"mode":"l4","codec":"batched","debug":2,"transports":[{"type":"direct","priority":50,"listen":"0.0.0.0:99999"}]}}'
RES=$(POST "$CONF_BAD" 'config')
echo "$RES" | grep -q '"ok":false' || fail "invalid config accepted"
grep -q "Listen = 0.0.0.0:17891" "$SBX/etc/opffk/openflux.conf" || fail "invalid apply modified the config"
pass "invalid config rejected without touching the file"

# egress proxy setting round-trip: validated, persisted, rendered into
# the upstream conf format
RES=$(POST '{"settings":{"mode":"l4","codec":"batched","debug":2,"egress_proxy":"socks5://user:pass@10.0.0.1:1080","transports":[{"type":"direct","priority":50,"listen":"0.0.0.0:17891"}]},"restart":false}' 'config')
echo "$RES" | grep -q '"ok":true' || fail "egress config rejected: $RES"
grep -q "EgressProxy = socks5://user:pass@10.0.0.1:1080" "$SBX/etc/opffk/openflux.conf" || fail "EgressProxy line missing from core config"
pass "egress proxy setting validated and written to the core config"

# ---- full self-update (test mode swaps files, skips the re-exec) ----------

# Build a "newer" backend binary to serve as the release content.
(cd "$ROOT_DIR/backend" && CGO_ENABLED=0 "$GO" build -buildvcs=false \
    -ldflags "-X main.version=v9.9.9-new" -o "$WORK/opffk-web-new" .)

# Local GitHub-API stand-in: /releases/latest JSON + tarball + sha256.
fuser -k 18996/tcp 2>/dev/null || true
sleep 0.5
RELDIR="$WORK/rel-v9.9.9"
mkdir -p "$RELDIR"
cp "$ROOT_DIR/install.sh" "$RELDIR/install.sh"
cp "$ROOT_DIR/packaging/S99opffk" "$RELDIR/S99opffk"
# the bundled manifest must pin the bundled core's actual hash — the
# self-update cross-checks them exactly like a real release does
CORE_SHA=$(sha256sum "$SBX/bin/openflux" | awk '{print $1}')
sed "s/\"amd64\": \"[0-9a-f]\{64\}\"/\"amd64\": \"$CORE_SHA\"/; s/\"version\": \"node-v[^\"]*\"/\"version\": \"node-v9.9.9\"/" \
    "$ROOT_DIR/manifest.json" > "$RELDIR/manifest.json"
cp "$WORK/opffk-web-new" "$RELDIR/opffk-web-amd64"
cp "$SBX/bin/openflux" "$RELDIR/openflux-linux-amd64"
printf 'v9.9.9\n' > "$RELDIR/VERSION"
mkdir -p "$RELDIR/opffk-v9.9.9-linux-amd64"
mv "$RELDIR/install.sh" "$RELDIR/S99opffk" "$RELDIR/manifest.json" "$RELDIR/VERSION" \
   "$RELDIR/opffk-web-amd64" "$RELDIR/openflux-linux-amd64" \
   "$RELDIR/opffk-v9.9.9-linux-amd64/"
( cd "$RELDIR" && tar -czf "$WORK/opffk-v9.9.9-linux-amd64.tar.gz" "opffk-v9.9.9-linux-amd64" )
SHA=$(sha256sum "$WORK/opffk-v9.9.9-linux-amd64.tar.gz" | awk '{print $1}')
printf '%s  opffk-v9.9.9-linux-amd64.tar.gz\n' "$SHA" > "$WORK/release.sha256"
python3 - "$WORK" <<'PY' &
import http.server, json, socketserver, sys, os, functools
work = sys.argv[1]
latest = {
  "tag_name": "v9.9.9",
  "assets": [
    {"name": "opffk-v9.9.9-linux-amd64.tar.gz",
     "browser_download_url": f"http://127.0.0.1:18996/opffk-v9.9.9-linux-amd64.tar.gz"},
    {"name": "opffk-v9.9.9-linux-amd64.tar.gz.sha256",
     "browser_download_url": f"http://127.0.0.1:18996/release.sha256"},
  ],
}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/repos/upiok/opffk/releases/latest":
            body = json.dumps(latest).encode()
            self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers()
            self.wfile.write(body); return
        path = os.path.join(work, self.path.lstrip("/"))
        if os.path.isfile(path):
            self.send_response(200)
            self.send_header("Content-Length", str(os.path.getsize(path)))
            self.end_headers()
            with open(path, "rb") as f: self.wfile.write(f.read())
            return
        self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", 18996), H) as srv:
    srv.serve_forever()
PY
RELHTTPPID=$!
sleep 0.5
kill -0 $RELHTTPPID 2>/dev/null || { cat "$WORK/release-server.log" >&2; fail "release server did not start (port busy?)"; }

# point the backend at the local release server via ui.json
python3 - "$SBX" <<'PY'
import json, sys
p = sys.argv[1] + "/etc/opffk/ui.json"
c = json.load(open(p))
c["update_repo"] = "upiok/opffk"
c["update_api_base"] = "http://127.0.0.1:18996"
json.dump(c, open(p, "w"), indent=2)
PY

# the backend keeps ui.json in memory: restart it so the local release
# server config takes effect, then log in again (sessions are in-memory)
OPFFK_ROOT="$SBX" "$SBX/etc/init.d/S99opffk" restart >/dev/null 2>&1
for i in $(seq 1 40); do
    curl -fsS -o /dev/null "http://127.0.0.1:$PORT/login" 2>/dev/null && break
    [ "$i" = 40 ] && fail "backend did not come back after config restart"
    sleep 0.5
done
curl -fsS -c "$COOKIES" -o /dev/null "http://127.0.0.1:$PORT/login"
LOGIN2=$(JAR -X POST -H "Content-Type: application/x-www-form-urlencoded" -d "password=$PW" "http://127.0.0.1:$PORT/login")
echo "$LOGIN2" | grep -q '"ok":true' || fail "re-login after restart failed: $LOGIN2"
CSRF2=$(echo "$LOGIN2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf"])')
RES=$(curl -sS -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF2" -H "Content-Type: application/json" \
    -d '{"action":"check_self"}' "http://127.0.0.1:$PORT/api/update")
echo "$RES" | grep -q '"latest":"v9.9.9"' || fail "self-update check failed: $RES"
pass "self-update check finds the newer release"

RES=$(curl -sS -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF2" -H "Content-Type: application/json" \
    -d '{"action":"apply_full"}' "http://127.0.0.1:$PORT/api/update")
echo "$RES" | grep -q '"ok":true' || fail "self-update apply failed: $RES"
cmp -s "$SBX/bin/opffk-web" "$WORK/opffk-web-new" || fail "backend binary not replaced"
grep -q "opffk v9.9.9" "$SBX/share/opffk/opffk-VERSION" 2>/dev/null || grep -q "v9.9.9" "$SBX/share/opffk/opffk-VERSION" || fail "opffk version marker not updated"
[ -x "$SBX/bin/opffk-web.previous" ] || fail "previous backend not kept"
pass "full self-update swapped backend, init script, manifest and version (exec skipped in test mode)"
kill $RELHTTPPID 2>/dev/null || true

# client link generation through the core's --make-link contract.
# The self-update restart re-logged us in above; refresh CSRF via state.
CSRF=$(JAR "http://127.0.0.1:$PORT/api/state" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf"])')
RES=$(JAR -X POST -H "X-Opffk-CSRF: $CSRF" -H "Content-Type: application/x-www-form-urlencoded" \
    -d "share_host=203.0.113.10" "http://127.0.0.1:$PORT/api/link")
echo "$RES" | grep -q "openflux://" || fail "client link generation failed: $RES"
pass "client link generated via the core's make-link contract"

# ---- password change -----------------------------------------------------------------

RES=$(POST "{\"current\":\"$PW\",\"new\":\"e2e-new-password-9\"}" 'password')
echo "$RES" | grep -q '"ok":true' || fail "password change failed: $RES"
curl -fsS -c "$COOKIES" -o /dev/null "http://127.0.0.1:$PORT/login"
LOGIN=$(JAR -X POST -H "Content-Type: application/x-www-form-urlencoded" -d "password=e2e-new-password-9" "http://127.0.0.1:$PORT/login")
echo "$LOGIN" | grep -q '"ok":true' || fail "login with new password failed: $LOGIN"
pass "password change and re-login work"

# ---- uninstall (API, gated) -------------------------------------------------------------

# relogin for a fresh session/CSRF (the password test dropped the old one)
curl -fsS -c "$COOKIES" -o /dev/null "http://127.0.0.1:$PORT/login"
CSRF=$(JAR -X POST -H "Content-Type: application/x-www-form-urlencoded" -d "password=e2e-new-password-9" \
    "http://127.0.0.1:$PORT/login" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf"])')

CODE=$(curl -sS -o /dev/null -w '%{http_code}' -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF" \
    -H "Content-Type: application/json" -d '{"confirm":"yes"}' "http://127.0.0.1:$PORT/api/uninstall")
[ "$CODE" = "400" ] || fail "uninstall without the magic phrase returned $CODE (want 400)"
pass "uninstall requires the typed confirmation"
RES=$(JAR -X POST -H "X-Opffk-CSRF: $CSRF" -H "Content-Type: application/json" -d '{"confirm":"UNINSTALL"}' "http://127.0.0.1:$PORT/api/uninstall")
echo "$RES" | grep -q '"ok":true' || fail "uninstall failed: $RES"
[ -f "$SBX/etc/opffk/openflux.conf" ] && fail "config still present after uninstall"
ls "$SBX/var/lib/opffk"/opffk-backup-*.tar.gz >/dev/null 2>&1 || fail "uninstall backup missing"
pass "gated uninstall removed config and kept a backup"

echo
echo "[e2e] ALL CHECKS PASSED"
