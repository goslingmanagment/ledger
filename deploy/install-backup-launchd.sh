#!/usr/bin/env bash
# Ставит на мак копию базы каждые 6 часов: копия pull-backup.sh в ~/Backups/ledger и launchd-агент
# ru.gosling-agency.ledger-backup. Повторный запуск обновляет копию скрипта и агента.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
dest=${LEDGER_BACKUP_DIR:-$HOME/Backups/ledger}
label=ru.gosling-agency.ledger-backup
plist="$HOME/Library/LaunchAgents/$label.plist"
mkdir -p "$dest/logs"
chmod 700 "$dest"
cp "$repo/deploy/pull-backup.sh" "$dest/pull-backup.sh"
sed -e "s#__DEST__#$dest#g" "$repo/deploy/launchd/$label.plist" > "$plist"
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
printf 'installed %s → %s/pull-backup.sh every 6 h\n' "$plist" "$dest"
