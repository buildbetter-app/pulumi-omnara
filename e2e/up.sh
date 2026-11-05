#!/usr/bin/env bash
# Bring up the local Omnara control plane and mint a bearer token for the
# public API. Idempotent: safe to re-run; an existing valid token is reused.
#
# Outputs (under .state/, mode 0600 for secrets):
#   token     personal access token (omnara_pat_v1_...) of the test user
#   env       shell-sourceable: OMNARA_API_URL, OMNARA_ORG_ID, OMNARA_E2E_TOKEN_FILE
#   password  password of the test user (only needed to mint a fresh token)
#
# The token is never printed. Secrets never appear in process arguments: they
# travel through curl header files, stdin bodies, and short-lived env vars.
set -euo pipefail

E2E_DIR="$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")"
STATE_DIR="$E2E_DIR/.state"
PROJECT="omnara-pulumi-e2e"
COMPOSE_FILE="$E2E_DIR/compose.yaml"

PORT="${OMNARA_E2E_PORT:-47100}"
export OMNARA_E2E_PORT="$PORT"
BASE="http://127.0.0.1:${PORT}"       # must equal OMNARA_PUBLIC_URL in compose.yaml
API_URL="${BASE}/api/v1"
EMAIL="${OMNARA_E2E_EMAIL:-pulumi-e2e@example.test}"
DISPLAY_NAME="Pulumi E2E"
ORG_NAME="Pulumi E2E"

log() { printf '[up] %s\n' "$*" >&2; }
die() { printf '[up] ERROR: %s\n' "$*" >&2; exit 1; }

for tool in docker curl jq openssl awk ss; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

compose() { docker compose -f "$COMPOSE_FILE" -p "$PROJECT" "$@"; }

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
JAR="$WORK/jar"
AUTH_HDR="$WORK/auth.hdr"
CSRF_HDR="$WORK/csrf.hdr"

# http METHOD URL [curl args...] -> sets CODE, body in $WORK/body.
CODE=000
http() {
  local method="$1" url="$2"
  shift 2
  : > "$WORK/body"
  CODE="$(curl -sS --max-time 60 -o "$WORK/body" -w '%{http_code}' -X "$method" "$url" "$@")" || CODE=000
}

body_error() { jq -r '[.code // empty, .error // empty] | join(": ")' "$WORK/body" 2>/dev/null || true; }

# --- 1. Stack ------------------------------------------------------------------

running_services="$(compose ps --status running --services 2>/dev/null || true)"
if ! grep -qx api <<<"$running_services"; then
  if [[ -n "$(ss -ltnH "sport = :${PORT}")" ]]; then
    die "127.0.0.1:${PORT} is already in use by something other than project ${PROJECT}; set OMNARA_E2E_PORT"
  fi
fi

log "starting compose project ${PROJECT} (port ${PORT})"
if ! compose up -d >&2; then
  compose logs --tail 40 >&2 || true
  die "docker compose up failed"
fi

log "waiting for ${BASE}/healthz"
healthy=0
for _ in $(seq 1 90); do
  if curl -fsS --max-time 3 -o /dev/null "${BASE}/healthz" 2>/dev/null; then
    healthy=1
    break
  fi
  sleep 2
done
if [[ "$healthy" != 1 ]]; then
  compose logs --tail 40 api >&2 || true
  die "api did not become healthy within 180s"
fi

# --- 2. Auth helpers -----------------------------------------------------------

# Console email driver: auth emails are printed to the api log. These helpers
# find the newest one so the verification/reset token can be read without SMTP.
email_block_count() {
  local n
  n="$(compose logs --no-log-prefix api 2>/dev/null | grep -c '^--- Omnara auth email ---$' || true)"
  printf '%s' "${n:-0}"
}

last_email_block() {
  compose logs --no-log-prefix api 2>/dev/null | awk '
    /^--- Omnara auth email ---$/ { buf = ""; inb = 1 }
    inb { buf = buf $0 "\n" }
    /^--- end Omnara auth email ---$/ { inb = 0; last = buf }
    END { printf "%s", last }'
}

# wait_for_email BASELINE_COUNT -> prints the new email block (waits up to 30s).
wait_for_email() {
  local baseline="$1" now block
  for _ in $(seq 1 60); do
    now="$(email_block_count)"
    if (( now > baseline )); then
      block="$(last_email_block)"
      if grep -q "^To: ${EMAIL}\$" <<<"$block"; then
        printf '%s' "$block"
        return 0
      fi
    fi
    sleep 0.5
  done
  return 1
}

# public_post PATH JSON_FROM_STDIN : same-origin JSON POST that stores cookies.
public_post() {
  local path="$1"
  http POST "${BASE}${path}" -c "$JAR" \
    -H 'Content-Type: application/json' -H "Origin: ${BASE}" --data-binary @-
}

password_file="$STATE_DIR/password"
if [[ ! -s "$password_file" ]]; then
  (umask 077; printf 'E2e-Pw-%s!\n' "$(openssl rand -hex 12)" > "$password_file")
fi
PASSWORD="$(head -n1 "$password_file")"

has_session() { awk '$6 == "omnara_session" { found = 1 } END { exit !found }' "$JAR" 2>/dev/null; }

login() {
  : > "$JAR"
  public_post /api/auth/login <<<"$(E="$EMAIL" P="$PASSWORD" jq -n '{email: env.E, password: env.P}')"
  [[ "$CODE" == 200 ]] && has_session
}

signup_and_verify() {
  local baseline block token
  baseline="$(email_block_count)"
  : > "$JAR"
  public_post /api/auth/signup <<<"$(E="$EMAIL" jq -n '{email: env.E}')"
  [[ "$CODE" == 202 ]] || die "signup returned HTTP ${CODE}: $(body_error)"
  block="$(wait_for_email "$baseline")" || die "no auth email appeared in the api log within 30s"
  if grep -q 'already exists' <<<"$block"; then
    return 2   # account exists with a password we do not know
  fi
  token="$(grep -o 'verify-email?token=[0-9a-f]*' <<<"$block" | head -n1 | cut -d= -f2)"
  [[ -n "$token" ]] || die "verification token not found in the auth email"
  public_post /api/auth/email/verify \
    <<<"$(T="$token" P="$PASSWORD" N="$DISPLAY_NAME" jq -n '{token: env.T, password: env.P, display_name: env.N}')"
  [[ "$CODE" == 200 ]] && has_session || die "email verify returned HTTP ${CODE}: $(body_error)"
}

reset_password() {
  local baseline block token
  baseline="$(email_block_count)"
  : > "$JAR"
  public_post /api/auth/password/reset/request <<<"$(E="$EMAIL" jq -n '{email: env.E}')"
  [[ "$CODE" == 202 ]] || die "password reset request returned HTTP ${CODE}: $(body_error)"
  block="$(wait_for_email "$baseline")" || die "no reset email appeared in the api log within 30s"
  token="$(grep -o 'reset-password?token=[0-9a-f]*' <<<"$block" | head -n1 | cut -d= -f2)"
  [[ -n "$token" ]] || die "reset token not found in the auth email"
  public_post /api/auth/password/reset <<<"$(T="$token" P="$PASSWORD" jq -n '{token: env.T, password: env.P}')"
  [[ "$CODE" == 200 ]] && has_session || die "password reset returned HTTP ${CODE}: $(body_error)"
}

# establish_session: leaves a logged-in browser session (cookie jar) behind.
establish_session() {
  if login; then
    log "logged in as ${EMAIL}"
    return 0
  fi
  log "login failed (HTTP ${CODE}); signing up ${EMAIL} (email verification via api log)"
  local rc=0
  signup_and_verify || rc=$?
  if [[ "$rc" == 2 ]]; then
    log "account exists with an unknown password; resetting it via the api-log reset email"
    reset_password
  fi
  has_session || die "could not establish a browser session"
  log "session established"
}

# mint_token: needs a browser session; creating PATs with another API key is not allowed.
mint_token() {
  local csrf tmp
  csrf="$(awk '$6 == "omnara_csrf" { print $7 }' "$JAR")"
  [[ -n "$csrf" ]] || die "no CSRF cookie in the session"
  printf 'X-Omnara-Csrf: %s\n' "$csrf" > "$CSRF_HDR"
  http POST "${API_URL}/personal-access-tokens" -b "$JAR" \
    -H 'Content-Type: application/json' -H "Origin: ${BASE}" -H "@${CSRF_HDR}" \
    --data-binary "{\"name\":\"pulumi-e2e-$(date -u +%Y%m%dT%H%M%SZ)\"}"
  [[ "$CODE" == 201 ]] || die "create personal access token returned HTTP ${CODE}: $(body_error)"
  tmp="$(umask 077; mktemp "$STATE_DIR/token.XXXXXX")"
  jq -r '.token' "$WORK/body" > "$tmp"
  [[ -s "$tmp" && "$(head -c 14 "$tmp")" == "omnara_pat_v1_" ]] || { rm -f "$tmp"; die "unexpected token response shape"; }
  chmod 600 "$tmp"
  mv "$tmp" "$STATE_DIR/token"
  log "minted a new personal access token"
}

# --- 3. Token ------------------------------------------------------------------

token_file="$STATE_DIR/token"
use_token() { printf 'Authorization: Bearer %s\n' "$(head -n1 "$token_file")" > "$AUTH_HDR"; }

token_ok=0
if [[ -s "$token_file" ]]; then
  use_token
  http GET "${API_URL}/me" -H "@${AUTH_HDR}"
  case "$CODE" in
    200) token_ok=1; log "existing token is valid" ;;
    401) log "existing token rejected (HTTP 401); minting a new one" ;;
    *) die "GET /me with the existing token returned HTTP ${CODE}" ;;
  esac
fi
if [[ "$token_ok" != 1 ]]; then
  establish_session
  mint_token
  use_token
  http GET "${API_URL}/me" -H "@${AUTH_HDR}"
  [[ "$CODE" == 200 ]] || die "GET /me with the new token returned HTTP ${CODE}: $(body_error)"
fi

# --- 4. Organization -----------------------------------------------------------

find_org() {
  jq -r --arg n "$ORG_NAME" \
    '(.orgs // []) | map(select(.name == $n and .role == "owner")) | .[0].id // empty' "$WORK/body"
}

ORG_ID="$(find_org)"
if [[ -z "$ORG_ID" ]]; then
  log "no '${ORG_NAME}' org owned by ${EMAIL}; creating one"
  http POST "${API_URL}/orgs" -H "@${AUTH_HDR}" -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: pulumi-e2e-org-v1' \
    --data-binary "$(jq -n --arg n "$ORG_NAME" '{name: $n}')"
  [[ "$CODE" == 201 || "$CODE" == 200 ]] || die "create org returned HTTP ${CODE}: $(body_error)"
  ORG_ID="$(jq -r '.org.id' "$WORK/body")"
fi
[[ "$ORG_ID" =~ ^org_[a-z2-7]{26}$ ]] || die "unexpected org id: ${ORG_ID}"

# --- 5. Verify and publish -----------------------------------------------------

http GET "${API_URL}/orgs/${ORG_ID}/projects" -H "@${AUTH_HDR}"
[[ "$CODE" == 200 ]] || die "list projects returned HTTP ${CODE}: $(body_error)"

env_tmp="$(umask 077; mktemp "$STATE_DIR/env.XXXXXX")"
cat > "$env_tmp" <<EOF
OMNARA_API_URL=${API_URL}
OMNARA_ORG_ID=${ORG_ID}
OMNARA_E2E_TOKEN_FILE=${STATE_DIR}/token
EOF
chmod 600 "$env_tmp"
mv "$env_tmp" "$STATE_DIR/env"

log "ready"
printf 'OMNARA_API_URL=%s\nOMNARA_ORG_ID=%s\ntoken file: %s/token (not printed)\nenv file:   %s/env\n' \
  "$API_URL" "$ORG_ID" "$STATE_DIR" "$STATE_DIR"
