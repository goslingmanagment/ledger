#!/usr/bin/env bash
# Учение на VPS: восстановить базу из реплики Litestream во временный файл и проверить его.
#   cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/replica-drill.sh
set -Eeuo pipefail
repo=$(git rev-parse --show-toplevel)
cd "$repo"
source "$repo/deploy/common.sh"
load_current_release
out="/data/backups/replica-drill-$(date -u +%Y%m%dT%H%M%SZ).db"
dc exec -T app mkdir -p /data/backups
dc exec -T litestream litestream restore -config /etc/litestream.yml -o "$out" /data/ledger.db
result=$(dc exec -T app ledger verify "$out")
dc exec -T app rm -f "$out"
jq -n --argjson verify "$result" '{ok:true,replicaRestore:$verify}'
