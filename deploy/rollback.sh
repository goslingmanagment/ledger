#!/usr/bin/env bash
# Вернуть релиз, который работал до последнего deploy.sh (из deploy/.runtime/last-backup.json):
#   cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/rollback.sh [--restore-backup]
# Без флага база остаётся как есть (миграции только добавляют, прежний код их не замечает). С флагом база
# возвращается к копии перед деплоем; всё, что записано после неё, уходит в /data/quarantine/ и
# собирается заново сборщиками, но решения владельца за это время придётся повторить.
set -Eeuo pipefail
umask 077
restore=false
case ${1:-} in --restore-backup) restore=true ;; '') ;; --help) sed -n '2,6p' "$0"; exit 0 ;; *) exit 1 ;; esac
repo=$(git rev-parse --show-toplevel)
cd "$repo"
source "$repo/deploy/common.sh"
runtime=$(runtime_directory)
manifest="$runtime/last-backup.json"
[[ -f $manifest ]] || { printf 'No %s: nothing to roll back to.\n' "$manifest" >&2; exit 1; }
exec 9>"$runtime/deploy.lock"
flock -n 9 || { printf 'Another deployment holds the lock.\n' >&2; exit 1; }
load_current_release
dc stop -t 20 app litestream
export LEDGER_IMAGE LEDGER_BUILD LEDGER_DEPLOYMENT LEDGER_RELEASE_DIR LEDGER_COMPOSE_FILE LITESTREAM_REPLICA_URL
LEDGER_IMAGE=$(jq -er .image "$manifest")
LEDGER_BUILD=$(jq -er .build "$manifest")
LEDGER_DEPLOYMENT=$(jq -er .deployment "$manifest")
LEDGER_RELEASE_DIR=$(jq -er .release "$manifest")
LEDGER_COMPOSE_FILE="$LEDGER_RELEASE_DIR/compose.yml"
LITESTREAM_REPLICA_URL=$(jq -er .replicaUrl "$manifest")
verify_release_snapshot "$LEDGER_RELEASE_DIR"
if [[ $restore == true ]]; then
  install_backup "$(jq -er .backup "$manifest")" >&2
  LITESTREAM_REPLICA_URL="${LITESTREAM_REPLICA_URL%/}-restore-$(date -u +%Y%m%dT%H%M%SZ)"
fi
start_release open
git checkout --quiet --detach "$(jq -er .build "$manifest")" || true
dc exec -T app ledger probe --print
