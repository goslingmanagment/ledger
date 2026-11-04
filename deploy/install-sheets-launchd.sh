#!/usr/bin/env bash
# Ставит на мак сборщик таблиц выплат: бинарник ledger в ~/.config/ledger/bin и launchd-агент
# ru.gosling-agency.ledger-sheets, раз в час и при входе. Повторный запуск обновляет оба.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
dest=${LEDGER_CONFIG_DIR:-$HOME/.config/ledger}
label=ru.gosling-agency.ledger-sheets
plist="$HOME/Library/LaunchAgents/$label.plist"
for need in sheets.json credentials; do
  [ -f "$dest/$need" ] || { printf 'нет %s/%s: см. deploy/README.md, раздел «Таблицы выплат»\n' "$dest" "$need" >&2; exit 2; }
done
mkdir -p "$dest/bin" "$dest/logs"
chmod 700 "$dest"
(cd "$repo" && CGO_ENABLED=0 go build -o "$dest/bin/ledger" ./cmd/ledger)
sed -e "s#__DEST__#$dest#g" "$repo/deploy/launchd/$label.plist" > "$plist"
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
printf 'installed %s → %s/bin/ledger collect sheets every hour\n' "$plist" "$dest"
