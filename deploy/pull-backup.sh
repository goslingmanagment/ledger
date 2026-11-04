#!/usr/bin/env bash
# Копия рабочей базы на мак: ~/Backups/ledger/<UTC>/ledger.db (+ health.json). Launchd на маке запускает
# копию этого файла каждые 6 часов (deploy/install-backup-launchd.sh); после правки — переустановить.
#   bash deploy/pull-backup.sh
# На сервере: согласованная копия VACUUM INTO внутри контейнера (ledger backup), она же передаётся по ssh и
# удаляется. Хранятся последние LEDGER_BACKUP_KEEP (120 = 30 дней) датированных копий; другое не трогается.
set -Eeuo pipefail
umask 077
host=${LEDGER_HOST:-root@152.53.191.242}
repo_on_host=${LEDGER_REMOTE_REPO:-/opt/ledger}
remote_env=${LEDGER_REMOTE_ENV:-/etc/ledger/ledger.env}
keep=${LEDGER_BACKUP_KEEP:-120}
stamp=$(date -u +%Y%m%dT%H%M%SZ)
base=${LEDGER_BACKUP_DIR:-$HOME/Backups/ledger}
target="$base/$stamp"
incoming="$base/.incoming-$stamp"
mkdir -p "$incoming"
trap 'rm -rf "$incoming"' EXIT
remote_copy="/data/backups/pull-$stamp.db"
app="cd '$repo_on_host' && set -a && . '$remote_env' && set +a && bash deploy/compose.sh exec -T app"
ssh -o BatchMode=yes "$host" "$app ledger backup '$remote_copy' >/dev/null && $app cat '$remote_copy' \
  && $app rm -f '$remote_copy'" > "$incoming/ledger.db"
ssh -o BatchMode=yes "$host" "curl -fsS http://127.0.0.1:3300/health" > "$incoming/health.json" || true
size=$(wc -c < "$incoming/ledger.db" | tr -d ' ')
[[ $size -gt 4096 ]] || { printf 'Backup looks empty (%s bytes), not kept\n' "$size" >&2; exit 1; }
if command -v sqlite3 >/dev/null; then
  check=$(sqlite3 "file:$incoming/ledger.db?mode=ro" 'PRAGMA integrity_check;')
  [[ $check == ok ]] || { printf 'Backup fails integrity_check, not kept: %s\n' "$check" >&2; exit 1; }
fi
# Датированное имя получает только проверенная копия: из них берутся учение восстановления и подсчёт хранения.
mv "$incoming" "$target"
# Отметка для проверки «копии базы»; не критична, копия уже на маке.
ssh -o BatchMode=yes "$host" "$app ledger mark-backup" || true
copies=("$base"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z/)
printf '%s\n' "${copies[@]}" | sort -r | tail -n +"$((keep + 1))" | while read -r old; do rm -rf "$old"; done
printf '{"ok":true,"path":"%s","dbBytes":%s}\n' "$target" "$size"
