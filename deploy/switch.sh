#!/usr/bin/env bash
# Ночь переключения: канон журнала переезжает из файла в /OS в ledger. Только после «да» владельца.
#   bash deploy/switch.sh check                                          проверки, ничего не меняет
#   LEDGER_TG_CHAT_ID=<чат> bash deploy/switch.sh go --aligns <файл>     переключение
#   bash deploy/switch.sh back                                           откат в тень, старые рутины снова работают
# Файл выравниваний лежит вне репозитория: по строке «счёт@ГГГГ-ММ-ДД[#N]=сумма» на каждый счёт, который сверка
# приводит к факту (баланс Bybit API, анкоры бота). Разница уходит в opening, список показывается владельцу заранее.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
host=${LEDGER_HOST:-root@152.53.191.242}
os_dir=${LEDGER_OS_DIR:-$HOME/fansly/OS}
dest=${LEDGER_CONFIG_DIR:-$HOME/.config/ledger}
bin=$dest/bin/ledger
collector=${LEDGER_COLLECTOR_CREDENTIALS:-$dest/credentials.collector}
old_agent=com.gosling.bybit-health
journal=finance/ledger.md
app="cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/compose.sh exec -T app"

server() { ssh -n -o BatchMode=yes "$host" "$app $*"; }
say() { printf '\n== %s\n' "$*"; }
die() { printf 'СТОП: %s\n' "$*" >&2; exit 1; }

check() {
  say "мак: всё, что нужно заданиям"
  for need in "$collector" "$dest/credentials" "$dest/credentials.bobr" "$dest/credentials.decide" "$dest/cards.json" "$dest/sheets.json" "$bin"; do
    [ -e "$need" ] || die "нет $need"
  done
  [ -d "$os_dir/.git" ] || die "нет репозитория $os_dir"
  command -v zsh >/dev/null || die "нет zsh"
  say "шлюз OpenClaw"
  openclaw cron list --json >/dev/null || die "шлюз OpenClaw не отвечает"
  say "сервер: режим, ключи сбора"
  [ "$(server ledger mode)" = shadow ] || die "режим сервера не shadow"
  ssh -n -o BatchMode=yes "$host" 'grep -q "^LEDGER_BYBIT_API_KEY=." /etc/ledger/ledger.env && grep -q "^LEDGER_HUB_AGENT_KEY=." /etc/ledger/ledger.env' ||
    die "на сервере нет ключей Bybit и хаба"
  say "API с мака"
  "$bin" sql "SELECT 1" >/dev/null || die "API недоступен: DNS, nginx или ключ"
  say "журнал: зафиксирован и совпадает с экспортом"
  [ -z "$(git -C "$os_dir" status --porcelain -- "$journal")" ] || die "$journal не закоммичен в /OS"
  LEDGER_CREDENTIALS=$collector "$bin" journal sync --git "$os_dir" >/dev/null
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "экспорт не совпадает с файлом в /OS"
  say "проверки: касса и карты выравниваются этой ночью, остальное должно быть зелёным"
  local report code=0 bad
  report=$(server ledger health --now) || code=$?
  printf '%s\n' "$report"
  [ "$code" = 0 ] || [ "$code" = 3 ] || die "проверки не отработали, код $code"
  printf '%s\n' "$report" | grep -q '^прогон:' || die "проверки не дали отчёта"
  bad=$(printf '%s\n' "$report" | grep -E '^(СБОЙ|СРОЧНО)' | grep -vE 'касса Bybit|карты против бота' || true)
  [ -z "$bad" ] || die "есть сбои кроме кассы и карт"
  say "можно переключать"
}

go() {
  local aligns=""
  [ "${1:-}" = --aligns ] && aligns=${2:-}
  [ -f "$aligns" ] || die "нужен файл выравниваний: --aligns <файл>"
  : "${LEDGER_TG_CHAT_ID:?нужен LEDGER_TG_CHAT_ID}"
  check
  trap 'printf "\nСТОП на шаге выше. Режим смотри: ssh на сервер, ledger mode. Вернуть всё: bash deploy/switch.sh back\n" >&2' ERR
  say "финальный импорт с выравниваниями"
  local work flags="" line
  work=$(mktemp -d)
  "$bin" journal bundle --git "$os_dir" > "$work/bundle.json"
  ssh -o BatchMode=yes "$host" "$app sh -c 'mkdir -p /data/import && cat > /data/import/bundle.json'" < "$work/bundle.json"
  rm -rf "$work"
  while IFS= read -r line; do
    [ -n "$line" ] && flags+=" --align '$line'"
  done < "$aligns"
  server "ledger import ledger-md /data/import/bundle.json $flags"
  server rm -rf /data/import
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "экспорт после импорта не совпадает, режим не менялся"
  say "OpenClaw: новые рутины, старые выключены"
  zsh "$repo/deploy/openclaw/install.sh"
  zsh "$repo/deploy/openclaw/install.sh" --retire-old
  launchctl bootout "gui/$(id -u)/$old_agent" 2>/dev/null || true
  say "мак: зеркало вместо толчка журнала"
  bash "$repo/deploy/install-mac-jobs.sh" live
  say "боевой режим: последний шаг, всё остальное уже на месте"
  server ledger mode live
  say "итог"
  server ledger health --now || true
  printf '\nДальше руками: python3 ~/.config/ledger/switch/os_edits.py --apply, validate и коммит /OS; сверка ledger month show с month.py на один срез.\n'
}

back() {
  say "тень: ledger больше не пишет строк"
  server ledger mode shadow
  say "мак: последнее зеркало, чтобы ни одна строка ledger не потерялась"
  LEDGER_CREDENTIALS=$dest/credentials "$bin" mirror --out "$os_dir/$journal"
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "зеркало не совпало с экспортом: старый контур не включён"
  if [ -n "$(git -C "$os_dir" status --porcelain -- "$journal")" ]; then
    git -C "$os_dir" add -- "$journal"
    git -C "$os_dir" commit -q -m "chore(finance): зеркало ledger перед откатом на файл" -- "$journal"
  fi
  say "мак: толчок журнала вместо зеркала"
  bash "$repo/deploy/install-mac-jobs.sh" shadow
  say "OpenClaw: старые рутины включены, новые выключены"
  zsh "$repo/deploy/openclaw/install.sh" --restore-old
  [ ! -f "$HOME/Library/LaunchAgents/$old_agent.plist" ] || launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/$old_agent.plist" 2>/dev/null || true
  printf '\nГотово: файл в /OS снова канон. Правки /OS этой ночи верни их обратным коммитом.\n'
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
  check) check ;;
  go) go "$@" ;;
  back) back ;;
  *) printf 'команды: check, go --aligns <файл>, back\n' >&2; exit 2 ;;
esac
