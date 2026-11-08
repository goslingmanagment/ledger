#!/usr/bin/env bash
# Ночь переключения: канон журнала переезжает из файла в /OS в ledger. Только после «да» владельца.
#   bash deploy/switch.sh check                                          проверки, ничего не меняет
#   LEDGER_TG_CHAT_ID=<чат> bash deploy/switch.sh go --aligns <файл>     переключение
#   bash deploy/switch.sh back [--os-commit <sha>]                       откат в тень, старые рутины снова работают
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
claw_agents=${LEDGER_CLAW_AGENTS:-$HOME/.openclaw/workspace/AGENTS.md}
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

# Откат идёт шагами; при сбое ловушка печатает, какие шаги сделаны, а какие нет. Шаги повторяемы: после
# исправления причины back запускают снова.
back_steps=(
  "проверки: всё, что нужно откату, на месте"
  "зеркало журнала в /OS, пока ledger в боевом режиме"
  "тень: ledger больше не пишет строк"
  "последнее зеркало журнала в /OS и коммит"
  "мак: толчок журнала вместо зеркала"
  "OpenClaw: старые рутины включены, новые выключены"
  "AGENTS.md OpenClaw как до переключения"
)
back_done=0

back_step() { # номер шага
  back_done=$1
  say "${back_steps[$1]}"
}

back_report() {
  local code=$? i mark
  trap - EXIT
  [ "$code" = 0 ] && return 0
  printf '\nСТОП: откат прерван на шаге «%s» (код %s).\n' "${back_steps[$back_done]}" "$code" >&2
  for i in "${!back_steps[@]}"; do
    mark="не сделано"
    [ "$i" -lt "$back_done" ] && mark="сделано"
    [ "$i" = "$back_done" ] && mark="СБОЙ"
    printf '  %s — %s\n' "$mark" "${back_steps[$i]}" >&2
  done
  if [ "$back_done" -gt 2 ]; then
    printf 'ledger уже в тени, файл в /OS ещё не канон. Исправь причину и снова: bash deploy/switch.sh back\n' >&2
    printf 'Остаться в боевом режиме: ssh %s и ledger mode live (контейнер app).\n' "$host" >&2
  else
    printf 'Сервер не менялся, ledger в боевом режиме. Исправь причину и снова: bash deploy/switch.sh back\n' >&2
  fi
  exit "$code"
}

mirror_commit() { # сообщение коммита
  LEDGER_CREDENTIALS=$dest/credentials "$bin" mirror --out "$os_dir/$journal"
  if [ -n "$(git -C "$os_dir" status --porcelain -- "$journal")" ]; then
    git -C "$os_dir" add -- "$journal"
    git -C "$os_dir" commit -q -m "$1" -- "$journal"
  fi
}

# claw_copy — копия AGENTS.md OpenClaw до переключения: LEDGER_CLAW_AGENTS_COPY или самая свежая в $dest/switch/<дата>/.
claw_copy() {
  if [ -n "${LEDGER_CLAW_AGENTS_COPY:-}" ]; then
    printf '%s' "$LEDGER_CLAW_AGENTS_COPY"
    return
  fi
  { find "$dest/switch" -mindepth 2 -maxdepth 2 -name openclaw-AGENTS.before-switch.md 2>/dev/null || true; } | sort | tail -n 1
}

back() {
  local os_commit="" copy saved mode
  [ "${1:-}" = --os-commit ] && os_commit=${2:-}
  trap back_report EXIT

  back_step 0
  for need in "$bin" "$dest/credentials" "$collector" "$dest/cards.json" "$dest/sheets.json"; do
    [ -e "$need" ] || die "нет $need"
  done
  [ -d "$os_dir/.git" ] || die "нет репозитория $os_dir"
  (cd "$os_dir" && [ ! -e "$(git rev-parse --git-path index.lock)" ]) || die "в $os_dir висит index.lock: другая сессия git"
  git -C "$os_dir" var GIT_COMMITTER_IDENT >/dev/null || die "git в $os_dir не знает автора коммита"
  command -v zsh >/dev/null || die "нет zsh"
  openclaw cron list --json >/dev/null || die "шлюз OpenClaw не отвечает"
  mode=$(server ledger mode)
  printf 'режим сервера: %s\n' "$mode"
  "$bin" sql "SELECT 1" >/dev/null || die "API недоступен: DNS, nginx или ключ"

  # Зеркало до тени: если коммит в /OS не пройдёт, сервер ещё не тронут.
  back_step 1
  mirror_commit "chore(finance): зеркало ledger перед откатом на файл"

  back_step 2
  server ledger mode shadow

  back_step 3
  mirror_commit "chore(finance): зеркало ledger перед откатом на файл"
  server ledger export ledger-md | cmp - "$os_dir/$journal" || die "зеркало не совпало с экспортом: старый контур не включён"

  back_step 4
  bash "$repo/deploy/install-mac-jobs.sh" shadow

  back_step 5
  zsh "$repo/deploy/openclaw/install.sh" --restore-old
  [ ! -f "$HOME/Library/LaunchAgents/$old_agent.plist" ] || launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/$old_agent.plist" 2>/dev/null || true

  back_step 6
  copy=$(claw_copy)
  if [ -z "$copy" ] || [ ! -f "$copy" ]; then
    printf 'копии нет (%s/switch/<дата>/openclaw-AGENTS.before-switch.md): правки этой ночи в %s верни руками\n' "$dest" "$claw_agents"
  elif cmp -s "$copy" "$claw_agents"; then
    printf 'уже как в копии %s\n' "$copy"
  else
    saved="$(dirname "$copy")/openclaw-AGENTS.before-back.md"
    [ ! -f "$claw_agents" ] || cp -p "$claw_agents" "$saved"
    cp "$copy" "$claw_agents"
    printf 'возвращён из %s; прежний сохранён: %s\n' "$copy" "$saved"
    printf 'что менялось с переключения (не только правки этой ночи — перенеси нужное): diff %s %s\n' "$copy" "$saved"
  fi
  back_done=${#back_steps[@]}
  trap - EXIT

  printf '\nГотово: файл в /OS снова канон, ledger в тени, старые рутины работают.\n'
  printf 'Руками — правки /OS этой ночи (os_edits.py --apply) откат не возвращает:\n'
  if [ -n "$os_commit" ]; then
    printf '  git -C %s revert --no-edit %s\n' "$os_dir" "$os_commit"
  else
    printf '  git -C %s log --oneline -5 -- skills/finance/SKILL.md   найти коммит правок\n' "$os_dir"
    printf '  git -C %s revert --no-edit <коммит>\n' "$os_dir"
  fi
  printf '  bash %s/scripts/validate.sh, затем git -C %s push\n' "$os_dir" "$os_dir"
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
  check) check ;;
  go) go "$@" ;;
  back) back "$@" ;;
  *) printf 'команды: check, go --aligns <файл>, back [--os-commit <sha>]\n' >&2; exit 2 ;;
esac
