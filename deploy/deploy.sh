#!/usr/bin/env bash
# Обновление работающей установки на VPS (первая установка — bootstrap.sh). Обычно его вызывает release.sh:
#   cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/deploy.sh [--plan] --image REF COMMIT
# Порядок: проверка метки образа → снимок релиза → стоп app → синхронизация реплики → копия базы →
# миграции новым образом → старт → /health с новой сборкой. Сбой до здорового /health возвращает прежний
# релиз; если миграции уже прошли — с базой из копии (данные сборщиков после копии собираются заново).
set -Eeuo pipefail
umask 077
image='' plan=false build_here=false target_ref=''
while (($#)); do
  case $1 in
    --image) image=${2:?--image requires a reference or an archive}; shift 2 ;;
    --plan) plan=true; shift ;;
    --build-here) build_here=true; shift ;;
    --help) sed -n '2,6p' "$0"; exit 0 ;;
    --*) printf 'Unknown option: %s\n' "$1" >&2; exit 1 ;;
    *) [[ -z $target_ref ]] || { printf 'One commit only.\n' >&2; exit 1; }; target_ref=$1; shift ;;
  esac
done
[[ -n $target_ref ]] || { printf 'Usage: deploy.sh [--plan] --image REF COMMIT\n' >&2; exit 1; }
repo=$(git rev-parse --show-toplevel)
cd "$repo"
source "$repo/deploy/common.sh"
target=$(git rev-parse --verify --quiet "$target_ref^{commit}") || { printf 'Unknown commit: %s\n' "$target_ref" >&2; exit 1; }
runtime=$(runtime_directory)
# release.sh переключает checkout на цель до вызова и передаёт, откуда он пришёл.
previous_ref=${LEDGER_PREVIOUS_REF:-$(git symbolic-ref -q --short HEAD || git rev-parse HEAD)}
if [[ $plan == true ]]; then
  current=null
  [[ ! -f $runtime/current.json ]] || current=$(jq -c . "$runtime/current.json")
  ref=${image:-ledger:$target}
  label=$(image_revision "$ref" || true)
  jq -n --arg target "$target" --arg previousRef "$previous_ref" --argjson current "$current" --arg image "$ref" \
    --arg label "${label:-}" --arg clean "$(git diff --quiet && git diff --cached --quiet && echo true || echo false)" \
    '{target:$target,previousRef:$previousRef,current:$current,image:{ref:$image,label:$label,matches:($label==$target)},
      checkoutClean:($clean=="true"),phases:["verify-image","snapshot","stop-app","confirmed-sync","stop-litestream",
      "backup","migrate","start","wait-health","confirmed-sync","save-current"]}'
  exit 0
fi
for tool in docker git jq flock; do command -v "$tool" >/dev/null || { printf 'Missing: %s\n' "$tool" >&2; exit 1; }; done
[[ $(uname -s) == Linux ]] || { printf 'Run deployments on the Linux VPS.\n' >&2; exit 1; }
git diff --quiet && git diff --cached --quiet || { printf 'Checkout contains changes; stopped.\n' >&2; exit 1; }
mkdir -p "$runtime"
exec 9>"$runtime/deploy.lock"
flock -n 9 || { printf 'Another deployment holds the lock.\n' >&2; exit 1; }
[[ -f $runtime/current.json ]] || { printf 'No current.json: first installation is bootstrap.sh.\n' >&2; exit 1; }
load_current_release
previous_image=$LEDGER_IMAGE previous_build=$LEDGER_BUILD previous_deployment=$LEDGER_DEPLOYMENT
previous_release=${LEDGER_RELEASE_DIR:?current.json names no release snapshot} previous_replica=$LITESTREAM_REPLICA_URL
verify_release_snapshot "$previous_release"
[[ -n $(dc ps -q app) ]] || { printf 'No running app.\n' >&2; exit 1; }
if [[ $build_here != true ]]; then resolve_image "$target" "$image"; fi
use_previous() {
  export LEDGER_IMAGE="$previous_image" LEDGER_BUILD="$previous_build" LEDGER_DEPLOYMENT="$previous_deployment" \
    LEDGER_RELEASE_DIR="$previous_release" LEDGER_COMPOSE_FILE="$previous_release/compose.yml" \
    LITESTREAM_REPLICA_URL="$previous_replica"
}
phase=before-stop backup_path=''
on_failure() {
  local code=$?
  trap - ERR
  case $phase in
    stopped)
      use_previous
      start_release open && printf 'Deployment stopped before migration; the previous release runs again.\n' >&2 ;;
    migrated)
      dc stop -t 20 app litestream || true
      use_previous
      quarantine=$(install_backup "$backup_path")
      # Старая база вернулась из копии: реплика начинает новую историю рядом, прежняя не трогается.
      export LITESTREAM_REPLICA_URL="${previous_replica%/}-restore-$(date -u +%Y%m%dT%H%M%SZ)"
      start_release open && printf 'Candidate failed after migration; previous release restored from %s (old files in %s).\n' \
        "$backup_path" "$quarantine" >&2 ;;
    serving)
      # Кандидат здоров и уже принимает записи: откат из копии потерял бы их. Реплику чинят рядом.
      printf 'The candidate is healthy and serving, but the replica did not confirm: it stays, check Litestream.\n' >&2
      exit "$code" ;;
    *) printf 'Deployment stopped before touching the running release.\n' >&2 ;;
  esac
  git checkout --quiet "$previous_ref" || printf 'Return the checkout to %s manually.\n' "$previous_ref" >&2
  exit "$code"
}
trap on_failure ERR
git checkout --quiet --detach "$target"
if [[ $build_here == true ]]; then
  LEDGER_COMPOSE_FILE="$repo/deploy/compose.yml" LEDGER_IMAGE="ledger:$target" LEDGER_BUILD="$target" dc_build
  resolve_image "$target"
fi
candidate_deployment="${target:0:12}-$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM"
candidate_release=$(LEDGER_IMAGE="ledger:$target" snapshot_release "$candidate_deployment" "$repo/deploy" checkout)
phase=stopped
dc stop -t 20 app
replica=$(confirmed_sync)
dc stop -t 40 litestream
backup_path="/data/backups/$(date -u +%Y%m%dT%H%M%SZ)-${target:0:12}.db"
metadata=$(dc run --rm --no-deps -T app ledger backup "$backup_path" </dev/null)
jq -n --arg image "$previous_image" --arg build "$previous_build" --arg deployment "$previous_deployment" \
  --arg release "$previous_release" --arg replicaUrl "$previous_replica" --arg backup "$backup_path" \
  --argjson database "$metadata" --argjson replication "$replica" \
  '{image:$image,build:$build,deployment:$deployment,release:$release,replicaUrl:$replicaUrl,backup:$backup,
    database:$database,replication:$replication}' > "$runtime/last-backup.json"
export LEDGER_IMAGE="ledger:$target" LEDGER_BUILD="$target" LEDGER_DEPLOYMENT="$candidate_deployment" \
  LEDGER_RELEASE_DIR="$candidate_release" LEDGER_COMPOSE_FILE="$candidate_release/compose.yml"
phase=migrated
schema=$(dc run --rm --no-deps -T app ledger migrate </dev/null | jq -er '.schemaVersion')
save_current_release candidate
dc up -d litestream app
wait_health "$target" "$schema"
save_current_release open
phase=serving
confirmed_sync >/dev/null
trap - ERR
prune_superseded_images "ledger:$target" "$previous_image" || true
dc exec -T app ledger probe --print
