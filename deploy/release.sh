#!/usr/bin/env bash
# Выпуск одной командой с мака (по образцу TaskIndex):
#   bash deploy/release.sh [SHA|REF] [--plan] [--bootstrap] [--build-here]
# SHA по умолчанию — origin/main и должен быть в origin/main. Шаги: прогон CI на push этого коммита зелёный;
# VPS сам скачивает артефакт ledger-image-<sha> по короткой подписанной ссылке и грузит образ; коммит уходит
# в bare-репозиторий на VPS; копия базы на мак; checkout и deploy.sh (--plan, затем по-настоящему); вторая копия.
# --bootstrap — первая установка: bare-репозиторий, checkout и /etc/ledger/ledger.env создаются, если их нет,
# вместо deploy.sh работает bootstrap.sh. --build-here — образ собирается на VPS из этого коммита (пока
# GitHub Actions недоступны; гейт тогда — локальные проверки перед слиянием).
# Нужны git, gh (вход выполнен), curl и ssh; секретов не читает.
set -Eeuo pipefail
host=${LEDGER_HOST:-root@152.53.191.242}
repo_on_host=${LEDGER_REMOTE_REPO:-/opt/ledger}
bare_on_host=${LEDGER_REMOTE_BARE:-/opt/ledger.git}
remote_env=${LEDGER_REMOTE_ENV:-/etc/ledger/ledger.env}
workflow=${LEDGER_CI_WORKFLOW:-ci}
plan=false bootstrap=false build_here=false ref=''
for arg in "$@"; do
  case $arg in
    --plan) plan=true ;;
    --bootstrap) bootstrap=true ;;
    --build-here) build_here=true ;;
    --help | -h) sed -n '2,10p' "$0"; exit 0 ;;
    --*) printf 'Unknown option: %s\n' "$arg" >&2; exit 1 ;;
    *) [[ -z $ref ]] || { printf 'One ref only.\n' >&2; exit 1; }; ref=$arg ;;
  esac
done
say() { printf '\n== %s\n' "$*" >&2; }
remote() { ssh -o BatchMode=yes "$host" "$@"; }
cd "$(git rev-parse --show-toplevel)"
git fetch -q origin
sha=$(git rev-parse --verify "${ref:-origin/main}^{commit}")
git merge-base --is-ancestor "$sha" origin/main || { printf 'Commit %s is not on origin/main.\n' "$sha" >&2; exit 1; }
say "release ${sha:0:7}: $(git log -1 --format=%s "$sha")"
image="ledger:$sha"
load_ci_image() {
  local run_id state artifact_id slug url label
  say "CI ($workflow)"
  run_id=$(gh run list --workflow "$workflow" --commit "$sha" --event push --limit 1 --json databaseId --jq '.[0].databaseId')
  [[ -n $run_id ]] || { printf 'No %s push run for %s yet.\n' "$workflow" "$sha" >&2; exit 1; }
  state=$(gh run view "$run_id" --json status,conclusion --jq '.status + "/" + .conclusion')
  if [[ $state != completed/success ]]; then
    say "waiting for run $run_id ($state)"
    gh run watch "$run_id" --exit-status >&2 || { printf 'CI run %s is not green.\n' "$run_id" >&2; exit 1; }
  fi
  say "image $image"
  if remote "docker image inspect '$image' >/dev/null 2>&1"; then
    printf 'already loaded on %s\n' "$host" >&2
    return 0
  fi
  artifact_id=$(gh api "repos/{owner}/{repo}/actions/runs/$run_id/artifacts?per_page=100" \
    --jq "[.artifacts[] | select(.name == \"ledger-image-$sha\" and (.expired | not))] | max_by(.created_at) | .id // empty")
  [[ -n $artifact_id ]] || { printf 'Artifact ledger-image-%s missing or expired in run %s.\n' "$sha" "$run_id" >&2; exit 1; }
  slug=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  url=$(curl -fsSI -H "Authorization: Bearer $(gh auth token)" -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$slug/actions/artifacts/$artifact_id/zip" \
    | awk 'tolower($1) == "location:" { print $2 }' | tr -d '\r')
  [[ $url == https://* ]] || { printf 'No signed artifact URL.\n' >&2; exit 1; }
  remote "sha='$sha' url='$url' bash -s" <<'REMOTE'
set -Eeuo pipefail
dir="/var/tmp/ledger-release-$sha"
trap 'rm -rf "$dir"' EXIT
rm -rf "$dir" && mkdir -p "$dir" && cd "$dir"
curl -fsSL --max-time 1800 -o image.zip "$url"
unzip -o -q image.zip || python3 -m zipfile -e image.zip .
docker load -i "ledger-$sha.tar.gz" >/dev/null
REMOTE
  label=$(remote "docker image inspect '$image' --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}'")
  [[ $label == "$sha" ]] || { printf 'Loaded image is labelled %s, not %s.\n' "$label" "$sha" >&2; exit 1; }
}
if $build_here; then
  say "image $image is built on $host (--build-here, CI skipped)"
  how="--build-here"
else
  load_ci_image
  how="--image '$image'"
fi
if $bootstrap; then
  say "first installation layout on $host"
  remote "set -e
    [ -d '$bare_on_host' ] || git init -q --bare '$bare_on_host'
    install -d -m 700 /etc/ledger
    [ -f '$remote_env' ] || { umask 077; printf '%s\n' 'LEDGER_VOLUME=ledger_data' 'LITESTREAM_REPLICA_URL=file:///data/replica/ledger' > '$remote_env'; }
    chmod 600 '$remote_env'"
fi
say "bare repository $host:$bare_on_host"
git push -q "$host:$bare_on_host" "${sha}:refs/heads/main"
if $bootstrap; then
  remote "[ -d '$repo_on_host/.git' ] || git clone -q '$bare_on_host' '$repo_on_host'"
fi
deploy="cd '$repo_on_host' && set -a && . '$remote_env' && set +a && bash deploy/deploy.sh"
checkout="cd '$repo_on_host' && git fetch -q origin && git checkout -q --detach '$sha'"
if $plan; then
  remote "set -e; cd '$repo_on_host' && git fetch -q origin && $deploy --plan $how '$sha'"
  say "plan only: nothing deployed"
  exit 0
fi
if $bootstrap; then
  say "bootstrap on $host"
  remote "set -e; $checkout && cd '$repo_on_host' && set -a && . '$remote_env' && set +a && bash deploy/bootstrap.sh $how --commit '$sha'"
else
  say "backup before"
  bash deploy/pull-backup.sh
  say "checkout and plan on $host"
  previous=$(remote "git -C '$repo_on_host' rev-parse HEAD")
  remote "set -e; $checkout && $deploy --plan $how '$sha'"
  say "deploy"
  remote "set -e; export LEDGER_PREVIOUS_REF='$previous'; $deploy $how '$sha'"
fi
say "backup after"
bash deploy/pull-backup.sh
say "released"
remote "cat '$repo_on_host/deploy/.runtime/current.json'"
