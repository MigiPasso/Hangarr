#!/bin/sh
# Smoke test of compose.yml (run by `make deploy-check` in the check container).
set -eu
base=http://cleanarr:8080
fail() { echo "FAIL: $*" >&2; exit 1; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

[ "$(code "$base/healthz")" = 200 ] || fail "/healthz"
[ "$(code "$base/readyz")" = 200 ] || fail "/readyz"
curl -fsS "$base/login" | grep -q 'Sign in' || fail "login page (English by default)"
[ "$(code "$base/")" = 303 ] || fail "anonymous access must redirect to /login"
[ "$(code -X POST "$base/webhooks/tautulli")" = 404 ] || fail "webhook must be disabled without a secret"
curl -fsSI "$base/login" | grep -qi "content-security-policy" || fail "security headers"
echo "deploy check: HTTP OK"
