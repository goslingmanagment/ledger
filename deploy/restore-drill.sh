#!/usr/bin/env bash
# Учение восстановления на маке: свежая копия из ~/Backups/ledger (или указанный файл) → проверка
# целостности → сервис на копии без сборщиков → /health. Копия не меняется: работа идёт на её дубле.
#   bash deploy/restore-drill.sh [путь/к/ledger.db]
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
src=${1:-$(ls -1d "${LEDGER_BACKUP_DIR:-$HOME/Backups/ledger}"/*Z/ledger.db 2>/dev/null | sort | tail -n1)}
[[ -n $src && -f $src ]] || { printf 'No backup found.\n' >&2; exit 1; }
work=$(mktemp -d)
trap 'kill "${pid:-0}" 2>/dev/null || true; rm -rf "$work"' EXIT
cp "$src" "$work/ledger.db"
(cd "$repo" && go build -o "$work/ledger" ./cmd/ledger)
verify=$("$work/ledger" verify "$work/ledger.db")
port=$((20000 + RANDOM % 20000))
LEDGER_DB="$work/ledger.db" LEDGER_SCHEDULER=off LEDGER_ADDR="127.0.0.1:$port" LEDGER_DEPLOYMENT=restore-drill \
  "$work/ledger" serve 2>"$work/serve.log" &
pid=$!
for ((i=0; i<50; i++)); do
  if health=$("$work/ledger" probe --url "http://127.0.0.1:$port/health" --print 2>/dev/null); then break; fi
  sleep 0.2
done
[[ -n ${health:-} ]] || { cat "$work/serve.log" >&2; printf 'Service on the restored copy did not answer.\n' >&2; exit 1; }
jq -n --arg src "$src" --argjson verify "$verify" --argjson health "$health" '{ok:true,source:$src,verify:$verify,health:$health}'
