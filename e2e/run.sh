#!/usr/bin/env bash
# End-to-end test of the Omnara Pulumi provider against the self-hosted
# Omnara that up.sh starts. Every step's command and exit code goes to
# .state/runs/<timestamp>/transcript.txt, the run's artifact.
#
# Steps: create everything; preview and refresh with no changes; detect and
# repair out-of-band edits and deletes; update in place and by replacement;
# import existing objects; destroy and confirm the API no longer lists them.
#
# Usage: e2e/run.sh   (after e2e/up.sh; needs the built provider in provider/bin)
set -euo pipefail

E2E_DIR="$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")"
STATE_DIR="$E2E_DIR/.state"
PROGRAM="$E2E_DIR/program"
IMPORT_PROGRAM="$E2E_DIR/import-program"
IMPORT_CLI_PROGRAM="$E2E_DIR/import-cli-program"
export PATH="$HOME/.pulumi/bin:$PATH"
PULUMI="${PULUMI:-$(command -v pulumi)}"

[[ -f "$STATE_DIR/env" && -f "$STATE_DIR/token" ]] || { echo "run e2e/up.sh first" >&2; exit 1; }
[[ -x "$E2E_DIR/../provider/bin/pulumi-resource-omnara" ]] || { echo "build the provider first (make provider)" >&2; exit 1; }
# shellcheck disable=SC1091
source "$STATE_DIR/env"
OMNARA_API_KEY="$(cat "$STATE_DIR/token")"
export OMNARA_API_URL OMNARA_ORG_ID OMNARA_API_KEY

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="$STATE_DIR/runs/$RUN_ID"
mkdir -p "$RUN_DIR"
TRANSCRIPT="$RUN_DIR/transcript.txt"
export PULUMI_HOME="$STATE_DIR/pulumi-home"
export PULUMI_BACKEND_URL="file://$STATE_DIR/backend"
export PULUMI_CONFIG_PASSPHRASE="omnara-e2e"
export PULUMI_SKIP_UPDATE_CHECK=1 PULUMI_SKIP_CONFIRMATIONS=1
mkdir -p "$PULUMI_HOME" "$STATE_DIR/backend"

SUFFIX="r$(date -u +%s)"
STACK="e2e-$SUFFIX"
IMPORT_STACK="import-$SUFFIX"
IMPORT_CLI_STACK="import-cli-$SUFFIX"
WORK="$(mktemp -d)"
AUTH_HDR="$WORK/auth.hdr"
umask 077
printf 'Authorization: Bearer %s\n' "$OMNARA_API_KEY" > "$AUTH_HDR"
FAILED=0

log() { printf '%s\n' "$*" | tee -a "$TRANSCRIPT"; }

# step NAME EXPECT CMD... runs CMD, records output and exit code, and fails
# the run unless the exit code matches EXPECT ("0" or "nonzero").
step() {
  local name="$1" expect="$2"
  shift 2
  log ""
  log "=== $name"
  log "\$ $*"
  set +e
  "$@" 2>&1 | tee -a "$TRANSCRIPT"
  local rc=${PIPESTATUS[0]}
  set -e
  log "--- exit $rc (expected $expect)"
  if [[ "$expect" == 0 && $rc -ne 0 ]] || [[ "$expect" == nonzero && $rc -eq 0 ]]; then
    log "FAIL: $name"
    FAILED=1
    return 1
  fi
}

pulumi_main() { "$PULUMI" --cwd "$PROGRAM" --stack "$STACK" --non-interactive --color never "$@"; }
pulumi_import() { "$PULUMI" --cwd "$IMPORT_PROGRAM" --stack "$IMPORT_STACK" --non-interactive --color never "$@"; }
pulumi_cli() { "$PULUMI" --cwd "$IMPORT_CLI_PROGRAM" --stack "$IMPORT_CLI_STACK" --non-interactive --color never "$@"; }

api() {
  local method="$1" path="$2"
  shift 2
  curl -fsS --max-time 60 -H @"$AUTH_HDR" -H 'Content-Type: application/json' -X "$method" "$OMNARA_API_URL$path" "$@"
}

output() { pulumi_main stack output "$1" 2>/dev/null; }

cleanup() {
  local rc=$?
  set +e
  if pulumi_main stack ls --json 2>/dev/null | jq -e --arg s "$STACK" 'any(.[]; .name == $s)' >/dev/null; then
    log ""
    log "=== cleanup: destroy anything left in $STACK"
    pulumi_main destroy --yes --skip-preview >>"$TRANSCRIPT" 2>&1
    pulumi_main stack rm --yes --force >>"$TRANSCRIPT" 2>&1
  fi
  pulumi_import stack rm --yes --force >/dev/null 2>&1
  pulumi_cli stack rm --yes --force >/dev/null 2>&1
  rm -rf "$WORK"
  if [[ $FAILED -ne 0 || $rc -ne 0 ]]; then
    log "RESULT: FAIL (see $TRANSCRIPT)"
    exit 1
  fi
  log "RESULT: PASS ($TRANSCRIPT)"
}
trap cleanup EXIT

log "Omnara Pulumi provider E2E run $RUN_ID"
log "api: $OMNARA_API_URL  org: $OMNARA_ORG_ID  stack: $STACK"
log "provider: $("$E2E_DIR/../provider/bin/pulumi-resource-omnara" -version 2>/dev/null || echo dev)  pulumi: $("$PULUMI" version)"

step "init stack" 0 pulumi_main stack init
step "set config" 0 pulumi_main config set suffix "$SUFFIX"

# 1. Create.
step "up: create every resource" 0 pulumi_main up --yes
step "preview: no changes after create" 0 pulumi_main preview --expect-no-changes
step "refresh: no drift after create" 0 pulumi_main refresh --yes --expect-no-changes

# Secret outputs must exist without being printed, and the API key must work.
# Tokens go through pipes, never through process arguments.
secret_set() { [[ "$(output "$1" --show-secrets | wc -c)" -gt 8 ]]; }
step "outputs: daemon token is set" 0 secret_set daemonToken

# A secret nested in a map input stays encrypted in state, after create and
# after a refresh that reads the plain value back from the API.
nested_secret_encrypted() {
  pulumi_main stack export | jq -e '
    [.deployment.resources[] | select(.type == "omnara:index:Machine")][0] as $m
    | ($m.outputs.env.API_TOKEN | type == "object" and has("ciphertext"))
      and ($m.inputs.env.API_TOKEN | type == "object" and has("ciphertext"))' >/dev/null &&
    ! pulumi_main stack export | grep -q e2e-nested-secret-value
}
step "state: nested secret is encrypted after create" 0 nested_secret_encrypted

PROJECT_ID="$(output projectId)"
MEMORY_ID="$(pulumi_main stack export | jq -r '.deployment.resources[] | select(.type=="omnara:index:MemoryStore") | .id')"
WEBHOOK_ID="$(output webhookId)"
SECRET_ID="$(output secretId)"
MODEL_PATH="/orgs/$OMNARA_ORG_ID/model-provider-configs/$(output modelProviderId)/models/$(output modelId)"

# A key the API (or anyone) adds to a map input is not the program's, so
# refresh ignores it. Omnara does this to Machine.metadata.
step "api: add a metadata key out of band" 0 \
  api PATCH "/orgs/$OMNARA_ORG_ID/secrets/$SECRET_ID" -d '{"metadata":{"purpose":"e2e","revision":"r1","added_elsewhere":"x"}}' -o /dev/null
step "preview --refresh: an added map key is not drift" 0 pulumi_main preview --refresh --expect-no-changes
step "refresh: state picks up the added key quietly" 0 pulumi_main refresh --yes
step "state: nested secret is encrypted after refresh" 0 nested_secret_encrypted

# 2. Drift: edit one object and delete another behind Pulumi's back.
step "api: edit the memory store out of band" 0 \
  api PATCH "/orgs/$OMNARA_ORG_ID/projects/$PROJECT_ID/memory-stores/$MEMORY_ID" -d '{"description":"edited outside Pulumi"}' -o /dev/null
step "api: delete the webhook out of band" 0 \
  api DELETE "/orgs/$OMNARA_ORG_ID/projects/$PROJECT_ID/event-webhooks/$WEBHOOK_ID" -o /dev/null
step "api: clear a nullable field out of band" 0 \
  api PUT "$MODEL_PATH" -d '{"max_output_tokens":null}' -o /dev/null
step "preview --refresh: drift is detected" nonzero pulumi_main preview --refresh --expect-no-changes
step "up --refresh: drift is repaired" 0 pulumi_main up --refresh --yes
step "preview: no changes after repair" 0 pulumi_main preview --expect-no-changes
step "api: memory store description restored" 0 bash -c \
  '[[ "$(curl -fsS -H @"$1" "$2" | jq -r .description)" == "E2E memory, revision 1." ]]' _ \
  "$AUTH_HDR" "$OMNARA_API_URL/orgs/$OMNARA_ORG_ID/projects/$PROJECT_ID/memory-stores/$MEMORY_ID"
step "api: cleared field restored" 0 bash -c \
  '[[ "$(curl -fsS -H @"$1" "${2%/*}?limit=100" | jq -r --arg id "${2##*/}" ".data[] | select(.id == \$id) | .max_output_tokens")" == "1000" ]]' _ \
  "$AUTH_HDR" "$OMNARA_API_URL$MODEL_PATH"

# 3. Update: revision 2 changes fields in place and replaces immutable ones.
CONFIG_BEFORE="$(output agentConfigId)"
SECRET_VERSION_BEFORE="$(output secretVersion)"
step "set revision 2" 0 pulumi_main config set revision 2
step "set revision 2 timeout" 0 pulumi_main config set requestTimeoutMs 20000
step "set revision 2 output tokens" 0 pulumi_main config set maxOutputTokens 2000
step "set revision 2 skill (renamed in SKILL.md)" 0 pulumi_main config set skillDir ./skill-renamed
step "preview: revision 2 plan" 0 pulumi_main preview --diff
step "up: apply revision 2" 0 pulumi_main up --yes
step "preview: no changes after revision 2" 0 pulumi_main preview --expect-no-changes
step "refresh: no drift after revision 2" 0 pulumi_main refresh --yes --expect-no-changes
step "update: agent profile moved to the new config" 0 bash -c \
  '[[ "$1" != "$2" && "$2" == "$3" ]]' _ "$CONFIG_BEFORE" "$(output agentConfigId)" "$(output agentProfileConfigId)"
step "update: secret material sent as a new version" 0 bash -c \
  '[[ "$2" -gt "$1" ]]' _ "$SECRET_VERSION_BEFORE" "$(output secretVersion)"
step "update: renamed skill was replaced" 0 bash -c '[[ "$1" == pulumi-e2e-notes-renamed ]]' _ "$(output skillName)"

# 4. Import existing objects into other stacks, then forget them.
SHARED_ID="$(output sharedProjectId)"
ACCESS_ID="$(output memberAccessId)"
PROFILE_ID="$(output agentProfileId)"
CONFIG_ID="$(output agentConfigId)"
WEBHOOK_ID="$(output webhookId)"
step "import cli: init stack" 0 pulumi_cli stack init
step "import cli: project by bare ID (org from provider)" 0 \
  pulumi_cli import omnara:index:Project shared "$SHARED_ID" --yes --protect=false --generate-code=false
step "import cli: secret by org/ID" 0 \
  pulumi_cli import omnara:index:Secret secret "$OMNARA_ORG_ID/$SECRET_ID" --yes --protect=false --generate-code=false
step "import cli: webhook by project/ID" 0 \
  pulumi_cli import omnara:index:EventWebhook webhook "$PROJECT_ID/$WEBHOOK_ID" --yes --protect=false --generate-code=false
step "import cli: composite-ID resource" 0 \
  pulumi_cli import omnara:index:OrgMemberProjectAccess access "$ACCESS_ID" --yes --protect=false --generate-code=false
step "import cli: imported inputs" 0 bash -c \
  '"$@" stack export | jq "[.deployment.resources[] | select(.custom) | {type, id, inputs}]"' \
  _ "$PULUMI" --cwd "$IMPORT_CLI_PROGRAM" --stack "$IMPORT_CLI_STACK" --non-interactive
step "import cli: inputs rebuilt from the API" 0 bash -c \
  '"$@" stack export | jq -e '"'"'[.deployment.resources[] | select(.custom and (.type | startswith("pulumi:providers:") | not)) | {type, inputs}] as $r | ($r | length) == 4
    and any($r[]; .type=="omnara:index:Project" and (.inputs.name | endswith("-shared")))
    and any($r[]; .type=="omnara:index:Secret" and .inputs.owner == {kind: "org"} and .inputs.metadata.purpose == "e2e")
    and any($r[]; .type=="omnara:index:OrgMemberProjectAccess" and .inputs.role == "admin")
    and any($r[]; .type=="omnara:index:EventWebhook" and .inputs.scope == {type: "project"})'"'"'' \
  _ "$PULUMI" --cwd "$IMPORT_CLI_PROGRAM" --stack "$IMPORT_CLI_STACK" --non-interactive
step "import cli: drop the stack without deleting" 0 pulumi_cli stack rm --yes --force

mkdir -p "$RUN_DIR/import-program"
sed -e "s#@PROVIDER_BIN@#$E2E_DIR/../provider/bin#" -e "s#@SUFFIX@#$SUFFIX#g" -e "s#@SHARED_ID@#$SHARED_ID#g" \
  -e "s#@SECRET_ID@#$SECRET_ID#g" -e "s#@ACCESS_ID@#$ACCESS_ID#g" -e "s#@PROJECT_ID@#$PROJECT_ID#g" \
  -e "s#@PROFILE_ID@#$PROFILE_ID#g" -e "s#@CONFIG_ID@#$CONFIG_ID#g" -e "s#@WEBHOOK_ID@#$WEBHOOK_ID#g" \
  "$IMPORT_PROGRAM/Pulumi.yaml.tmpl" > "$RUN_DIR/import-program/Pulumi.yaml"
IMPORT_PROGRAM="$RUN_DIR/import-program"
step "import option: init stack" 0 pulumi_import stack init
step "import option: up imports with matching inputs" 0 pulumi_import up --yes
step "import option: no changes after import" 0 pulumi_import preview --expect-no-changes
# Removing a map input from the program clears it.
sed -i '/^      metadata:$/,/^        revision: r2$/d' "$IMPORT_PROGRAM/Pulumi.yaml"
step "import option: removed metadata is cleared" 0 pulumi_import up --yes
step "api: secret metadata is empty" 0 bash -c \
  '[[ "$(curl -fsS -H @"$1" "$2" | jq -c .metadata)" == "{}" ]]' _ "$AUTH_HDR" "$OMNARA_API_URL/orgs/$OMNARA_ORG_ID/secrets/$SECRET_ID"
step "import option: no changes after clearing" 0 pulumi_import preview --expect-no-changes
step "import option: drop the stack without deleting" 0 pulumi_import stack rm --yes --force

# 5. Destroy.
step "destroy: delete every resource" 0 pulumi_main destroy --yes
step "api: projects are gone" 0 bash -c \
  '! curl -fsS -H @"$1" "$2?limit=100" | jq -e --arg a "$3" --arg b "$4" "any(.data[]; .id==\$a or .id==\$b)"' _ \
  "$AUTH_HDR" "$OMNARA_API_URL/orgs/$OMNARA_ORG_ID/projects" "$PROJECT_ID" "$SHARED_ID"
step "api: secret is gone" nonzero api GET "/orgs/$OMNARA_ORG_ID/secrets/$SECRET_ID" -o /dev/null
step "stack rm" 0 pulumi_main stack rm --yes
