#!/bin/sh
# tunnel_e2e.sh — end-to-end tunnel test with the REAL upstream OpenFlux core.
#
# Builds upstream from a local checkout, runs it as an exit node under the
# opffk backend (using the backend's generated openflux.conf and encryption
# key), then runs the same core as a SOCKS5 client and pushes an HTTP
# request through the tunnel to a local test server.
#
# This exercises the real OpenFlux protocol against the configuration the
# web backend writes — if the generated .conf, key handling, session
# context derivation or make-link integration were wrong, this test fails.
#
# Usage: GO=... OPFFK_UPSTREAM=/path/to/OpenFlux sh tests/tunnel_e2e.sh

set -eu

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
GO=${GO:-go}
UPSTREAM="${OPFFK_UPSTREAM:-$ROOT_DIR/../OpenFlux}"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/opffk-tunnel.XXXXXX")
SBX="$WORK/sbx"
WEB_PORT=18795
EXIT_PORT=17991
CLIENT_PORT=11080
HTTP_PORT=18999
# The gVisor stack treats loopback destinations as martians, so the tunnel
# target must be a real (non-127/8) address of this host.
HOSTIP=$(hostname -I 2>/dev/null | awk '{print $1}')
[ -n "$HOSTIP" ] || fail "cannot determine a non-loopback host address (hostname -I)"

HTTPPID=""
cleanup() {
    [ -n "$HTTPPID" ] && kill "$HTTPPID" 2>/dev/null || true
    [ -f "$SBX/var/run/opffk-web.pid" ] && kill "$(cat "$SBX/var/run/opffk-web.pid")" 2>/dev/null || true
    pkill -f "of-tunnel-core" 2>/dev/null || true
    if [ "${OPFFK_KEEP:-0}" = "1" ]; then
        echo "[tunnel] keeping workdir: $WORK"
        return 0
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

pass() { printf '[tunnel ok] %s\n' "$1"; }
fail() { printf '[tunnel FAIL] %s\n' "$1" >&2; exit 1; }

[ -d "$UPSTREAM" ] || fail "upstream checkout not found at $UPSTREAM (clone https://github.com/p1neappleXpress/OpenFlux)"

# ---- build the real core and the backend ---------------------------------------

echo "[tunnel] building upstream core (this can take a couple of minutes)"
cp -r "$UPSTREAM" "$WORK/upstream"
rm -rf "$WORK/upstream/.git"
(cd "$WORK/upstream" && CGO_ENABLED=0 "$GO" build -buildvcs=false -trimpath -o "$WORK/of-tunnel-core" .)

echo "[tunnel] building backend"
(cd "$ROOT_DIR/backend" && CGO_ENABLED=0 "$GO" build -buildvcs=false -ldflags "-X main.version=tunnel-e2e" -o "$WORK/opffk-web" .)

# ---- local HTTP server as the tunnel's destination -------------------------------

python3 - "$HTTP_PORT" > "$WORK/http.log" 2>&1 <<'EOF' &
import http.server, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"THROUGH-THE-TUNNEL"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
with socketserver.TCPServer(("0.0.0.0", int(sys.argv[1])), H) as srv:
    srv.serve_forever()
EOF
HTTPPID=$!

# ---- backend with the real core ---------------------------------------------------

mkdir -p "$SBX/bin"
cp "$WORK/of-tunnel-core" "$SBX/bin/openflux"
OPFFK_ROOT="$SBX" nohup "$WORK/opffk-web" -listen 127.0.0.1 -port $WEB_PORT > "$WORK/console.log" 2>&1 &
echo $! > "$SBX/pid"

for i in $(seq 1 40); do
    curl -fsS -o /dev/null "http://127.0.0.1:$WEB_PORT/login" 2>/dev/null && break
    [ "$i" = 40 ] && { cat "$WORK/console.log"; fail "backend did not start"; }
    sleep 0.5
done
pass "backend up with the real core"

PW=$(head -1 "$SBX/etc/opffk/initial-password")
COOKIES="$WORK/cookies.txt"
curl -fsS -c "$COOKIES" -o /dev/null "http://127.0.0.1:$WEB_PORT/login"
LOGIN=$(curl -sS -b "$COOKIES" -c "$COOKIES" -X POST \
    -H "Content-Type: application/x-www-form-urlencoded" -d "password=$PW" \
    "http://127.0.0.1:$WEB_PORT/login")
CSRF=$(echo "$LOGIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf"])')

# stop the autostarted core (it has no config yet), write config, restart
curl -sS -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF" -H "Content-Type: application/json" \
    -d '{}' "http://127.0.0.1:$WEB_PORT/api/service/stop" > /dev/null

CFG="{\"settings\":{\"mode\":\"l4\",\"codec\":\"batched\",\"debug\":2,\"transports\":[{\"type\":\"direct\",\"priority\":50,\"listen\":\"127.0.0.1:$EXIT_PORT\"}]},\"restart\":true}"
RES=$(curl -sS -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF" -H "Content-Type: application/json" \
    -d "$CFG" "http://127.0.0.1:$WEB_PORT/api/config")
echo "$RES" | grep -q '"ok":true' || fail "config apply failed: $RES"
pass "backend wrote the core config and restarted the real core"

# the exit must announce itself in the core log (upstream prints
# "mode=proxy" for l4 — the proxy exit backend — and accepts on the port)
for i in $(seq 1 30); do
    if grep -q "Running as EXIT NODE" "$SBX/var/log/opffk/core.log" 2>/dev/null \
       && grep -q "acceptLoop: started on 127.0.0.1:$EXIT_PORT" "$SBX/var/log/opffk/core.log" 2>/dev/null; then break; fi
    [ "$i" = 30 ] && { cat "$SBX/var/log/opffk/core.log" 2>/dev/null; fail "exit node line not found in core log"; }
    sleep 0.5
done
pass "upstream core runs as exit node (mode=l4) on the generated config"

# ---- client link via the backend (core --make-link) --------------------------------

LINK_JSON=$(curl -sS -b "$COOKIES" -X POST -H "X-Opffk-CSRF: $CSRF" \
    -H "Content-Type: application/x-www-form-urlencoded" -d "share_host=127.0.0.1" \
    "http://127.0.0.1:$WEB_PORT/api/link")
echo "$LINK_JSON" > "$WORK/link.json" || true
LINK=$(echo "$LINK_JSON" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("link", d.get("error", "no-link")))' 2>/dev/null || echo no-link)
case "$LINK" in
    openflux://*) pass "client link generated" ;;
    *) echo "---- link response ----" >&2; cat "$WORK/link.json" >&2; fail "bad link: $LINK" ;;
esac

# ---- run the real client through the tunnel -----------------------------------------

cp "$SBX/etc/opffk/encryption-key" "$WORK/client.key"
chmod 600 "$WORK/client.key"

nohup "$WORK/of-tunnel-core" \
    --role=client --inbound=socks5 --socks5="127.0.0.1:$CLIENT_PORT" \
    --transport=direct --direct-dial="127.0.0.1:$EXIT_PORT" \
    --encryption-key-file="$WORK/client.key" \
    -dd > "$WORK/client.log" 2>&1 &
CLIENTPID=$!
sleep 3
kill -0 $CLIENTPID 2>/dev/null || { cat "$WORK/client.log"; fail "client exited immediately"; }

BODY=$(curl -fsS --max-time 20 --socks5-hostname "127.0.0.1:$CLIENT_PORT" "http://$HOSTIP:$HTTP_PORT/" 2>"$WORK/curl.err" || true)
if [ "$BODY" = "THROUGH-THE-TUNNEL" ]; then
    pass "HTTP request completed through the real OpenFlux tunnel (l4 exit -> SOCKS5 client)"
else
    echo "---- client log ----"; tail -30 "$WORK/client.log" >&2
    echo "---- curl err ----"; cat "$WORK/curl.err" >&2
    fail "tunnel did not carry the request (body='$BODY')"
fi

kill $CLIENTPID 2>/dev/null || true
kill $HTTPPID 2>/dev/null || true
echo
echo "[tunnel] REAL TUNNEL E2E PASSED"
