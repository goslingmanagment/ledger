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
bin=${LEDGER_BIN:-$HOME/.config/ledger/bin/ledger}
journal=finance/ledger.md
app="cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/compose.sh exec -T app"

server() { ssh -n -o BatchMode=yes "$host" "$app $*"; }
say() { printf '\n== %s\n' "$*"; }
die() { printf 'СТОП: %s\n' "$*" >&2; exit 1; }

check() {
  say "сервер: режим, ключи сбора"
  [ "$(server ledger mode)" = shadow ] || die "режим сервера не shadow"
  ssh -n -o BatchMode=yes "$host" 'grep -q "^LEDGER_BYBIT_API_KEY=." /etc/ledger/ledger.env && grep -q "^LEDGER_HUB_AGENT_KEY=." /etc/ledger/ledger.env' ||
    die "на сервере нет ключей Bybit и хаба"
  say "API с мака"
  "$bin" sql "SELECT 1" >/dev/null || die "API недоступен: DNS, nginx или ключ"
  say "журнал: зафиксирован и совпадает с экспортом"
  [ -z "$(git -C "$os_dir" status --porcelain -- "$journal")" ] || die "$journal не закоммичен в /OS"
  "$bin" journal sync --git "$os_dir" >/dev/null
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "экспорт не совпадает с файлом в /OS"
  say "проверки: касса и карты выравниваются этой ночью, остальное должно быть зелёным"
  local report bad
  report=$(server ledger health --now || true)
  printf '%s\n' "$report"
  bad=$(printf '%s\n' "$report" | grep -E '^(СБОЙ|СРОЧНО)' | grep -vE 'касса Bybit|карты против бота' || true)
  [ -z "$bad" ] || die "есть сбои кроме кассы и карт"
  say "шлюз OpenClaw"
  openclaw cron list --json >/dev/null || die "шлюз OpenClaw не отвечает"
  say "можно переключать"
}

go() {
  local aligns=""
  [ "${1:-}" = --aligns ] && aligns=${2:-}
  [ -f "$aligns" ] || die "нужен файл выравниваний: --aligns <файл>"
  : "${LEDGER_TG_CHAT_ID:?нужен LEDGER_TG_CHAT_ID}"
  check
  say "финальный импорт с выравниваниями"
  local work flags="" line
  work=$(mktemp -d)
  trap 'rm -rf "$work"' RETURN
  "$bin" journal bundle --git "$os_dir" > "$work/bundle.json"
  ssh -o BatchMode=yes "$host" "$app sh -c 'mkdir -p /data/import && cat > /data/import/bundle.json'" < "$work/bundle.json"
  while IFS= read -r line; do
    [ -n "$line" ] && flags+=" --align '$line'"
  done < "$aligns"
  server "ledger import ledger-md /data/import/bundle.json $flags"
  server rm -rf /data/import
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "экспорт после импорта не совпадает, режим не менялся"
  say "боевой режим"
  server ledger mode live
  say "мак: сборщики и зеркало вместо толчка журнала"
  bash "$repo/deploy/install-mac-jobs.sh" live
  say "OpenClaw: новые рутины, старые выключены"
  zsh "$repo/deploy/openclaw/install.sh"
  zsh "$repo/deploy/openclaw/install.sh" --retire-old
  launchctl bootout "gui/$(id -u)/com.gosling.bybit-health" 2>/dev/null || true
  say "итог"
  server ledger health --now || true
  printf '\nДальше руками: правки /OS и AGENTS.md OpenClaw (лежат вне репозитория), сверка ledger month show с month.py на один срез.\n'
}

back() {
  say "тень"
  server ledger mode shadow
  say "мак: толчок журнала вместо зеркала"
  bash "$repo/deploy/install-mac-jobs.sh" shadow
  say "OpenClaw: старые рутины включены, новые выключены"
  zsh "$repo/deploy/openclaw/install.sh" --restore-old
  printf '\nСтроки, которые ledger успел написать, есть в зеркале %s: закоммить его в /OS, иначе следующий импорт увидит расхождение.\n' "$os_dir/$journal"
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
  check) check ;;
  go) go "$@" ;;
  back) back ;;
  *) printf 'команды: check, go --aligns <файл>, back\n' >&2; exit 2 ;;
esac
