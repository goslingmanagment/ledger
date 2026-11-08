#!/bin/zsh
# Рутины ledger в OpenClaw: идемпотентно, по declarationKey. Джобы как код: промпты и условия рядом.
# Запуск (нужен живой Gateway и ~/.config/ledger/bin/ledger):
#   LEDGER_TG_CHAT_ID=<чат владельца> deploy/openclaw/install.sh            новые рутины ledger
#   deploy/openclaw/install.sh --retire-old                                  выключить рутины ledger-sync и month.py
#   deploy/openclaw/install.sh --restore-old                                 откат: старые включить, новые выключить
# Агент OpenClaw — LEDGER_OPENCLAW_AGENT (по умолчанию main): шлюз с несколькими агентами без --agent рутину не примет.
set -euo pipefail
HERE=${0:A:h}
OLD=(bybit-nightly:v2 bybit-health:v1 bybit-health-flush:v1 month-weekly:v1 month-close:v1)
NEW=(ledger-nightly:v1 ledger-health:v1 ledger-urgent:v1 ledger-month-weekly:v1 ledger-month-close:v1)

job_id() {
  openclaw cron list --all --json | python3 -c '
import json,sys
d=json.load(sys.stdin); jobs=d.get("jobs",d) if isinstance(d,dict) else d
m=[j["id"] for j in jobs if j.get("declarationKey")==sys.argv[1]]
assert len(m)<=1, "duplicate declaration"
print(m[0] if m else "")' "$1"
}

toggle() {  # enable|disable, declarationKey…
  local verb=$1 decl id
  shift
  for decl in "$@"; do
    id=$(job_id "$decl")
    if [[ -n "$id" ]]; then
      openclaw cron "$verb" "$id" >/dev/null
      echo "$verb: $decl ($id)"
    else
      echo "нет джоба: $decl"
    fi
  done
}

case ${1:-} in
  --retire-old) toggle disable "${OLD[@]}"; exit 0 ;;
  --restore-old) toggle enable "${OLD[@]}"; toggle disable "${NEW[@]}"; exit 0 ;;
  "") ;;
  *) echo "неизвестный ключ: $1" >&2; exit 2 ;;
esac

CHAT_ID=${LEDGER_TG_CHAT_ID:?нужен LEDGER_TG_CHAT_ID: чат владельца с ботом OpenClaw}

install_job() {  # declarationKey, имя, cron, таймаут, файл промпта, файл условия или "", описание
  local decl="$1" name="$2" cron="$3" timeout="$4" prompt="$5" trigger="$6" desc="$7" existing id
  existing=$(job_id "$decl")
  local -a common
  common=(
    --agent "${LEDGER_OPENCLAW_AGENT:-main}"
    --cron "$cron" --tz Europe/Moscow --exact
    --session isolated --wake now
    --announce --channel telegram --to "$CHAT_ID"
    --timeout-seconds "$timeout"
    --name "$name" --display-name "$name" --description "$desc"
    --message "$(<"$HERE/$prompt")"
  )
  if [[ -n "$trigger" ]]; then
    common+=(--light-context --tools exec,read --trigger-script "$HERE/$trigger")
  fi
  if [[ -n "$existing" ]]; then
    openclaw cron edit "$existing" "${common[@]}" >/dev/null
    id=$existing
  else
    id=$(openclaw cron add --declaration-key "$decl" "${common[@]}" --json |
      python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("id") or d.get("job",{}).get("id"))')
  fi
  openclaw cron edit "$id" --enable --failure-alert --failure-alert-mode announce --failure-alert-channel telegram \
    --failure-alert-to "$CHAT_ID" --failure-alert-after 1 --failure-alert-cooldown 6h >/dev/null
  echo "готово: $name ($id)"
}

install_job ledger-nightly:v1 "ledger: ночной бриф (00:30)" "30 0 * * *" 900 nightly.prompt.md "" \
  "Бриф из открытых предложений ledger. Ответы владельца применяет основная сессия через ledger decide. Джоб как код: deploy/openclaw в репозитории ledger."
install_job ledger-health:v1 "ledger: health (11:00)" "0 11 * * *" 300 health.prompt.md health.trigger.js \
  "Сбои проверок ledger строкой владельцу; всё зелёное — молчит. Джоб как код: deploy/openclaw в репозитории ledger."
install_job ledger-urgent:v1 "ledger: срочное (каждые 15 минут)" "*/15 * * * *" 300 urgent.prompt.md urgent.trigger.js \
  "Срочные сбои ledger приходят сразу; без новых сбоев модель не запускается. Джоб как код: deploy/openclaw в репозитории ledger."
install_job ledger-month-weekly:v1 "ledger: цифры месяца (пн 10:00)" "0 10 * * 1" 900 month-weekly.prompt.md "" \
  "Понедельничная строка цифр месяца из ledger month brief. Джоб как код: deploy/openclaw в репозитории ledger."
install_job ledger-month-close:v1 "ledger: закрытие месяца (10–18-го, 12:00)" "0 12 10-18 * *" 900 month-close.prompt.md "" \
  "Закрытие прошлого месяца через ledger month close, снимок в базе, вопросы владельцу. Месяц закрыт — молчит. Джоб как код: deploy/openclaw в репозитории ledger."
