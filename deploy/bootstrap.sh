#!/usr/bin/env bash
# Первая установка на VPS. Дальше — deploy.sh (состояние живёт в deploy/.runtime).
#   cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/bootstrap.sh --image REF [--commit SHA]
set -Eeuo pipefail
umask 077
image='' commit_ref=HEAD existing=false build_here=false
while (($#)); do
  case $1 in
    --image) image=${2:?}; shift 2 ;;
    --commit) commit_ref=${2:?}; shift 2 ;;
    --existing-database) existing=true; shift ;;
    --build-here) build_here=true; shift ;;
    --help) sed -n '2,3p' "$0"; exit 0 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done
repo=$(git rev-parse --show-toplevel)
cd "$repo"
source "$repo/deploy/common.sh"
for tool in docker git jq flock; do command -v "$tool" >/dev/null || { printf 'Missing: %s\n' "$tool" >&2; exit 1; }; done
[[ $(uname -s) == Linux ]] || { printf 'Run the installation on the Linux VPS.\n' >&2; exit 1; }
git diff --quiet && git diff --cached --quiet || { printf 'Checkout contains changes.\n' >&2; exit 1; }
target=$(git rev-parse --verify "$commit_ref^{commit}")
[[ $(git rev-parse HEAD) == "$target" ]] || { printf 'Checkout is not on %s.\n' "$target" >&2; exit 1; }
runtime=$(runtime_directory)
mkdir -p "$runtime"
exec 9>"$runtime/deploy.lock"
flock -n 9 || { printf 'Another deployment holds the lock.\n' >&2; exit 1; }
[[ ! -f $runtime/current.json ]] || { printf 'current.json exists: use deploy.sh.\n' >&2; exit 1; }
export LEDGER_IMAGE="ledger:$target" LEDGER_BUILD="$target"
export LEDGER_DEPLOYMENT="${target:0:12}-$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM"
export LITESTREAM_REPLICA_URL=${LITESTREAM_REPLICA_URL:-file:///data/replica/ledger}
unset LEDGER_RELEASE_DIR LEDGER_COMPOSE_FILE
dc config --quiet
[[ -z $(dc ps -q app) ]] || { printf 'An app container is running: use deploy.sh.\n' >&2; exit 1; }
if [[ $build_here == true ]]; then dc_build; resolve_image "$target"; else resolve_image "$target" "$image"; fi
if [[ $existing != true ]] && dc run --rm --no-deps -T app test -e /data/ledger.db </dev/null; then
  printf 'Volume already holds /data/ledger.db; pass --existing-database.\n' >&2
  exit 1
fi
failed() {
  local code=$?
  trap - ERR
  dc stop -t 20 app litestream 2>/dev/null || true
  printf 'Installation failed (%s); runtime state kept in %s.\n' "$LEDGER_DEPLOYMENT" "$runtime" >&2
  exit "$code"
}
trap failed ERR
dc run --rm --no-deps -T app ledger migrate </dev/null >&2
schema=$(dc run --rm --no-deps -T app ledger migrate </dev/null | jq -er '.schemaVersion')
release=$(snapshot_release "$LEDGER_DEPLOYMENT" "$repo/deploy" bootstrap)
export LEDGER_RELEASE_DIR="$release" LEDGER_COMPOSE_FILE="$release/compose.yml"
save_current_release candidate
dc up -d app litestream
wait_health "$target" "$schema"
confirmed_sync >/dev/null
save_current_release open
trap - ERR
jq -n --arg deployment "$LEDGER_DEPLOYMENT" --arg image "$LEDGER_IMAGE" --arg release "$release" \
  '{ok:true,deployment:$deployment,image:$image,release:$release,
    next:["nginx: deploy/nginx/ledger.conf → /etc/nginx/sites-available/ledger, certbot, nginx -t, reload",
          "check: curl -s http://127.0.0.1:3300/health; bash deploy/replica-drill.sh",
          "next releases: deploy/release.sh on the Mac"]}'
