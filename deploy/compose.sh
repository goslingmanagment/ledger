#!/usr/bin/env bash
# docker compose текущего релиза: образ, сборка и снимок конфигурации из deploy/.runtime/current.json.
set -Eeuo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
source "$repo/deploy/common.sh"
load_current_release
dc "$@"
