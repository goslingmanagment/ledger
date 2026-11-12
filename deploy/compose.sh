#!/usr/bin/env bash
# docker compose текущего релиза: образ, сборка и снимок конфигурации из deploy/.runtime/current.json.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
source "$repo/deploy/common.sh"
load_current_release
# Настройки и ключи сервиса (LEDGER_ENV_FILE, по умолчанию /etc/ledger/ledger.env) грузим сами, если их ещё нет в
# окружении: compose подставляет ключи из окружения, и `compose.sh up -d` без них молча пересоздавал контейнер с пустыми
# ключами хаба и Bybit (12.11.2026, полторы минуты). Уже загруженное окружение не трогаем.
env_file=${LEDGER_ENV_FILE:-/etc/ledger/ledger.env}
if [[ -z ${LEDGER_HUB_AGENT_KEY:-} && -r $env_file ]]; then
  set -a
  # shellcheck disable=SC1090
  . "$env_file"
  set +a
fi
dc "$@"
