#!/usr/bin/env bash
# Runs examples/typescript through the generated Node.js SDK against the
# self-hosted Omnara that up.sh starts: up, preview with no changes, destroy.
# The transcript goes to .state/runs/<timestamp>-typescript/transcript.txt.
#
# Usage: e2e/run-typescript-example.sh  (after e2e/up.sh, make provider, and
# make build-sdk-nodejs)
set -euo pipefail

E2E_DIR="$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")"
ROOT="$(dirname "$E2E_DIR")"
STATE_DIR="$E2E_DIR/.state"
EXAMPLE="$ROOT/examples/typescript"
export PATH="$HOME/.pulumi/bin:$PATH"
PULUMI="${PULUMI:-$(command -v pulumi)}"

[[ -f "$STATE_DIR/env" && -f "$STATE_DIR/token" ]] || { echo "run e2e/up.sh first" >&2; exit 1; }
[[ -f "$ROOT/sdk/nodejs/bin/index.js" ]] || { echo "build the Node.js SDK first (make build-sdk-nodejs)" >&2; exit 1; }
# shellcheck disable=SC1091
source "$STATE_DIR/env"
OMNARA_API_KEY="$(cat "$STATE_DIR/token")"
export OMNARA_API_URL OMNARA_ORG_ID OMNARA_API_KEY

RUN_DIR="$STATE_DIR/runs/$(date -u +%Y%m%dT%H%M%SZ)-typescript"
mkdir -p "$RUN_DIR"
TRANSCRIPT="$RUN_DIR/transcript.txt"
export PULUMI_HOME="$STATE_DIR/pulumi-home"
export PULUMI_BACKEND_URL="file://$STATE_DIR/backend"
export PULUMI_CONFIG_PASSPHRASE="omnara-e2e"
export PULUMI_SKIP_UPDATE_CHECK=1
mkdir -p "$PULUMI_HOME" "$STATE_DIR/backend"
STACK="ts-$(date -u +%s)"
VERSION="$("$PULUMI" package get-schema "$ROOT/provider/bin/pulumi-resource-omnara" | jq -r .version)"
FAILED=0

log() { printf '%s\n' "$*" | tee -a "$TRANSCRIPT"; }
step() {
  local name="$1"
  shift
  log ""
  log "=== $name"
  log "\$ $*"
  set +e
  "$@" 2>&1 | tee -a "$TRANSCRIPT"
  local rc=${PIPESTATUS[0]}
  set -e
  log "--- exit $rc (expected 0)"
  if [[ $rc -ne 0 ]]; then
    log "FAIL: $name"
    FAILED=1
    return 1
  fi
}
pulumi_ex() { "$PULUMI" --cwd "$EXAMPLE" --stack "$STACK" --non-interactive --color never "$@"; }

cleanup() {
  local rc=$?
  set +e
  if pulumi_ex stack ls --json 2>/dev/null | jq -e --arg s "$STACK" 'any(.[]; .name == $s)' >/dev/null; then
    log ""
    log "=== cleanup: destroy $STACK"
    pulumi_ex destroy --yes --skip-preview >>"$TRANSCRIPT" 2>&1
    pulumi_ex stack rm --yes --force >>"$TRANSCRIPT" 2>&1
  fi
  if [[ $FAILED -ne 0 || $rc -ne 0 ]]; then
    log "RESULT: FAIL (see $TRANSCRIPT)"
    exit 1
  fi
  log "RESULT: PASS ($TRANSCRIPT)"
}
trap cleanup EXIT

log "TypeScript example against $OMNARA_API_URL (org $OMNARA_ORG_ID), provider $VERSION"
step "install the local plugin" "$PULUMI" plugin install resource omnara "$VERSION" \
  --file "$ROOT/provider/bin/pulumi-resource-omnara" --reinstall
step "npm install" npm --prefix "$EXAMPLE" install --no-audit --no-fund --loglevel=error
step "init stack" pulumi_ex stack init
# A dummy key and a closed loopback port keep the run offline. Self-hosted
# Omnara in dev mode accepts loopback model URLs.
step "config: model key" bash -c 'printf "%s" "sk-ant-e2e-dummy" | "$@"' _ "$PULUMI" --cwd "$EXAMPLE" --stack "$STACK" config set --secret anthropicApiKey
step "config: model url" pulumi_ex config set modelBaseUrl http://127.0.0.1:9/v1
step "up" pulumi_ex up --yes
step "preview: no changes" pulumi_ex preview --expect-no-changes
step "refresh: no drift" pulumi_ex refresh --yes --expect-no-changes
step "destroy" pulumi_ex destroy --yes
step "stack rm" pulumi_ex stack rm --yes
