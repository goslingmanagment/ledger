#!/usr/bin/env bash
# Ставит на мак задания ledger: бинарник в ~/.config/ledger/bin и launchd-агенты из одного шаблона, журналы в ~/Library/Logs/ledger.
#   bash deploy/install-mac-jobs.sh shadow   сборщики и толчок коммитов журнала на сервер: канон — файл в /OS
#   bash deploy/install-mac-jobs.sh live     сборщики и зеркало журнала в /OS: канон — ledger
# Сборщики: карты раз в 30 минут, посты «Отчётности» раз в 15 минут (если есть reports.json), таблицы выплат раз в час;
# журнал раз в час. Повторный запуск обновляет всё.
set -Eeuo pipefail
mode=${1:-}
case "$mode" in shadow | live) ;; *) printf 'нужен режим: shadow или live\n' >&2; exit 2 ;; esac
repo=$(cd "$(dirname "$0")/.." && pwd)
dest=${LEDGER_CONFIG_DIR:-$HOME/.config/ledger}
logs=${LEDGER_LOG_DIR:-$HOME/Library/Logs/ledger}
credentials=${LEDGER_COLLECTOR_CREDENTIALS:-$dest/credentials.collector}
os_dir=${LEDGER_OS_DIR:-$HOME/fansly/OS}
template="$repo/deploy/launchd/ru.gosling-agency.ledger-job.plist"

for need in "$credentials" "$dest/cards.json" "$dest/sheets.json"; do
  [ -f "$need" ] || { printf 'нет %s: см. deploy/README.md, раздел «Задания на маке»\n' "$need" >&2; exit 2; }
done
[ -d "$os_dir/.git" ] || { printf 'нет репозитория %s: задайте LEDGER_OS_DIR\n' "$os_dir" >&2; exit 2; }
mkdir -p "$dest/bin" "$logs"
chmod 700 "$dest"
(cd "$repo" && CGO_ENABLED=0 go build -o "$dest/bin/ledger" ./cmd/ledger)

install_agent() { # имя, интервал в секундах, аргументы ledger…
  local name=$1 interval=$2 label="ru.gosling-agency.ledger-$1" args="" a
  shift 2
  for a in "$@"; do args+="<string>$a</string>"; done
  local plist="$HOME/Library/LaunchAgents/$label.plist"
  sed -e "s#__LABEL__#$label#g" -e "s#__ARGS__#$args#g" -e "s#__NAME__#$name#g" -e "s#__INTERVAL__#$interval#g" -e "s#__DEST__#$dest#g" \
    -e "s#__HOME__#$HOME#g" -e "s#__LOGS__#$logs#g" -e "s#__CREDENTIALS__#$credentials#g" "$template" > "$plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  printf 'installed %s: ledger %s, every %s s\n' "$label" "$*" "$interval"
}

remove_agent() { # имя
  local label="ru.gosling-agency.ledger-$1" plist="$HOME/Library/LaunchAgents/ru.gosling-agency.ledger-$1.plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  rm -f "$plist"
}

install_agent cards 1800 collect cards
install_agent sheets 3600 collect sheets
if [ -f "$dest/reports.json" ]; then
  install_agent reports 900 collect reports
else
  remove_agent reports
  printf 'skipped reports: нет %s (см. deploy/README.md, «Задания на маке»)\n' "$dest/reports.json"
fi
if [ "$mode" = shadow ]; then
  remove_agent mirror
  install_agent journal 3600 journal sync --git "$os_dir"
else
  remove_agent journal
  install_agent mirror 3600 mirror --out "$os_dir/finance/ledger.md"
fi
