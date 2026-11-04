#!/usr/bin/env bash
# Ставит на мак сборщики: бинарник ledger в ~/.config/ledger/bin и launchd-агенты из одного шаблона —
# карты CinCin раз в 30 минут, таблицы выплат раз в час (если команда и конфиг есть), оба при входе.
# Журналы в ~/Library/Logs/ledger. Повторный запуск обновляет всё.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
dest=${LEDGER_CONFIG_DIR:-$HOME/.config/ledger}
logs=${LEDGER_LOG_DIR:-$HOME/Library/Logs/ledger}
credentials=${LEDGER_COLLECTOR_CREDENTIALS:-$dest/credentials.collector}
template="$repo/deploy/launchd/ru.gosling-agency.ledger-collect.plist"

need() {
  [ -f "$1" ] || { printf 'нет %s: см. deploy/README.md, раздел «Сборщики на маке»\n' "$1" >&2; exit 2; }
}
need "$credentials"
need "$dest/cards.json"
mkdir -p "$dest/bin" "$logs"
chmod 700 "$dest"
(cd "$repo" && CGO_ENABLED=0 go build -o "$dest/bin/ledger" ./cmd/ledger)

install_agent() { # метка, что собирать, интервал в секундах
  local label=$1 what=$2 interval=$3 plist="$HOME/Library/LaunchAgents/$1.plist"
  sed -e "s#__LABEL__#$label#g" -e "s#__WHAT__#$what#g" -e "s#__INTERVAL__#$interval#g" -e "s#__DEST__#$dest#g" \
    -e "s#__HOME__#$HOME#g" -e "s#__LOGS__#$logs#g" -e "s#__CREDENTIALS__#$credentials#g" "$template" > "$plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  printf 'installed %s → ledger collect %s every %s s\n' "$plist" "$what" "$interval"
}

install_agent ru.gosling-agency.ledger-cards cards 1800
if "$dest/bin/ledger" help 2>&1 | grep -q 'collect sheets' && [ -f "$dest/sheets.json" ]; then
  install_agent ru.gosling-agency.ledger-sheets sheets 3600
else
  printf 'таблицы выплат пропущены: нет команды collect sheets или %s/sheets.json\n' "$dest"
fi
