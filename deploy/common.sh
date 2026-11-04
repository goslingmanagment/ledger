#!/usr/bin/env bash
# Общие функции деплоя (по образцу TaskIndex). Подключать из скрипта, где задан repo. Секретов нет.
: "${repo:?common.sh requires repo}"
runtime_directory() { printf '%s\n' "${LEDGER_RUNTIME_DIR:-$repo/deploy/.runtime}"; }
# compose-файл работающего релиза: его снимок, если current.json его называет, иначе checkout.
compose_file() { printf '%s\n' "${LEDGER_COMPOSE_FILE:-$repo/deploy/compose.yml}"; }
load_current_release() {
  local state release
  state="$(runtime_directory)/current.json"
  if [[ -f $state ]]; then
    export LEDGER_IMAGE LEDGER_BUILD LEDGER_DEPLOYMENT LITESTREAM_REPLICA_URL
    LEDGER_IMAGE=$(jq -er '.image' "$state")
    LEDGER_BUILD=$(jq -er '.build' "$state")
    LEDGER_DEPLOYMENT=$(jq -er '.deployment' "$state")
    LITESTREAM_REPLICA_URL=$(jq -er '.replicaUrl' "$state")
    release=$(jq -r '.release // empty' "$state")
    if [[ -n $release ]]; then
      [[ -f $release/compose.yml ]] || {
        printf 'Release snapshot %s named in %s is missing.\n' "$release" "$state" >&2
        return 1
      }
      export LEDGER_RELEASE_DIR="$release" LEDGER_COMPOSE_FILE="$release/compose.yml"
    fi
  fi
}
dc() {
  local args=(-f "$(compose_file)")
  if [[ -n ${LEDGER_COMPOSE_PROJECT:-} ]]; then args=(-p "$LEDGER_COMPOSE_PROJECT" "${args[@]}"); fi
  if [[ -n ${LEDGER_COMPOSE_OVERRIDE:-} ]]; then args+=(-f "$LEDGER_COMPOSE_OVERRIDE"); fi
  docker compose "${args[@]}" "$@"
}
dc_build() { dc -f "$repo/deploy/compose.build.yml" build app; }
image_id() { docker image inspect --format '{{.Id}}' "$1" 2>/dev/null || printf 'null'; }
image_revision() {
  docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$1" 2>/dev/null
}
# save_current_release STATUS: образ, сборка, деплой и реплика из окружения, снимок из LEDGER_RELEASE_DIR.
save_current_release() {
  local dir tmp
  dir=$(runtime_directory)
  mkdir -p "$dir"
  tmp="$dir/current.json.$$"
  jq -n --arg image "$LEDGER_IMAGE" --arg build "$LEDGER_BUILD" --arg deployment "$LEDGER_DEPLOYMENT" \
    --arg replicaUrl "$LITESTREAM_REPLICA_URL" --arg status "$1" --arg release "${LEDGER_RELEASE_DIR:-}" \
    --arg imageId "$(image_id "$LEDGER_IMAGE")" \
    '{image:$image,build:$build,deployment:$deployment,replicaUrl:$replicaUrl,status:$status,
      release:(if $release == "" then null else $release end),
      imageId:(if $imageId == "null" then null else $imageId end)}' > "$tmp"
  mv "$tmp" "$dir/current.json"
}
file_sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
release_files=(compose.yml litestream.yml common.sh)
# snapshot_release DEPLOYMENT SOURCE_DIR LABEL: копия конфигурации релиза с sha256 каждого файла.
snapshot_release() {
  local deployment=$1 src=$2 label=$3 target files name commit
  target="$(runtime_directory)/releases/$deployment"
  [[ ! -e $target ]] || { printf 'Release snapshot already exists: %s\n' "$target" >&2; return 1; }
  mkdir -p "$target"
  files='{}'
  for name in "${release_files[@]}"; do
    [[ -f $src/$name ]] || { printf 'Missing release file: %s/%s\n' "$src" "$name" >&2; return 1; }
    cp "$src/$name" "$target/$name"
    # litestream.yml монтируется в контейнер с uid 1000: снимок пишется под umask 077.
    [[ $name != litestream.yml ]] || chmod 644 "$target/$name"
    files=$(jq --arg n "$name" --arg s "$(file_sha256 "$target/$name")" '. + {($n): $s}' <<<"$files")
  done
  commit=$(git -C "$src" rev-parse HEAD 2>/dev/null || printf 'unknown')
  jq -n --arg deployment "$deployment" --arg commit "$commit" --arg image "${LEDGER_IMAGE:-}" \
    --arg createdAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg source "$label" --argjson files "$files" \
    '{deployment:$deployment,commit:$commit,image:$image,createdAt:$createdAt,source:$source,files:$files}' \
    > "$target/release.json"
  printf '%s\n' "$target"
}
verify_release_snapshot() {
  local dir=$1 name expected
  [[ -f $dir/release.json ]] || { printf 'No release.json in %s\n' "$dir" >&2; return 1; }
  for name in "${release_files[@]}"; do
    expected=$(jq -er --arg n "$name" '.files[$n]' "$dir/release.json") || return 1
    [[ $(file_sha256 "$dir/$name") == "$expected" ]] || {
      printf 'Release file %s/%s changed since the snapshot\n' "$dir" "$name" >&2; return 1; }
  done
}
# resolve_image TARGET [REF|ARCHIVE]: сделать ledger:TARGET доступным; отказ, если метка revision не TARGET.
resolve_image() {
  local target=$1 ref=${2:-} loaded label
  if [[ -n $ref && -f $ref ]]; then
    loaded=$(docker load --quiet --input "$ref") || return 1
    ref=$(sed -n 's/^Loaded image: //p' <<<"$loaded" | head -n1)
    [[ -n $ref ]] || { printf 'Archive %s contains no tagged image\n' "$2" >&2; return 1; }
  fi
  [[ -n $ref ]] || ref="ledger:$target"
  docker image inspect "$ref" >/dev/null 2>&1 || {
    printf 'Image %s is not present: transfer it from CI (release.sh) or --build-here.\n' "$ref" >&2
    return 1
  }
  label=$(image_revision "$ref")
  [[ $label == "$target" ]] || {
    printf 'Image %s carries revision "%s", expected %s; refusing.\n' "$ref" "$label" "$target" >&2
    return 1
  }
  [[ $ref == "ledger:$target" ]] || docker tag "$ref" "ledger:$target"
}
# wait_health BUILD [SCHEMA]: /health отвечает ok с этой сборкой (и схемой) за 30 с.
wait_health() {
  local build=$1 schema=${2:-} expect=("build=$1")
  [[ -z $schema ]] || expect+=("schemaVersion=$schema")
  for ((attempt=0; attempt<30; attempt++)); do
    if dc exec -T app ledger probe "${expect[@]}" 2>/dev/null; then return 0; fi
    sleep 1
  done
  printf 'The app did not report healthy build=%s %s within 30 s.\n' "$build" "$schema" >&2
  return 1
}
confirmed_sync() {
  local point
  for ((attempt=0; attempt<10; attempt++)); do
    if point=$(dc exec -T litestream litestream sync -wait -json -socket /data/litestream.sock /data/ledger.db) && \
      jq -e '.replica_txid != null and .replica_txid == .txid' <<<"$point" >/dev/null; then
      printf '%s\n' "$point"
      return 0
    fi
    sleep 1
  done
  return 1
}
# prune_superseded_images KEEP_REF...: убрать образы ledger:<sha>, кроме названных и работающих.
prune_superseded_images() {
  [[ -z ${LEDGER_COMPOSE_PROJECT:-} ]] || return 0
  local ref id tag removed=0
  declare -A keep=()
  for ref in "$@"; do id=$(image_id "$ref"); [[ $id == null ]] || keep["$id"]=1; done
  while IFS= read -r id; do [[ -z $id ]] || keep["$id"]=1; done \
    < <(docker ps -q 2>/dev/null | xargs -r docker inspect --format '{{.Image}}' 2>/dev/null)
  while IFS= read -r tag; do
    [[ $tag =~ ^ledger:[a-f0-9]{40}$ ]] || continue
    id=$(image_id "$tag")
    [[ $id != null && -z ${keep[$id]:-} ]] || continue
    docker rmi "$tag" >/dev/null 2>&1 && removed=$((removed + 1))
  done < <(docker images --format '{{.Repository}}:{{.Tag}}' ledger 2>/dev/null)
  printf 'Image prune: removed %s superseded ledger image tag(s).\n' "$removed" >&2
}
# install_backup BACKUP: подменить /data/ledger.db копией; прежняя база и состояние Litestream уходят в
# /data/quarantine/<UTC>/ (ничего не удаляется). Только при остановленных app и litestream.
install_backup() {
  local backup=$1 quarantine
  quarantine="/data/quarantine/$(date -u +%Y%m%dT%H%M%SZ)"
  dc run --rm --no-deps -T app sh -euc '
    q=$1; b=$2; test -s "$b"; mkdir -p "$q"
    for f in /data/ledger.db /data/ledger.db-wal /data/ledger.db-shm /data/.ledger.db-litestream; do
      if [ -e "$f" ]; then mv "$f" "$q/"; fi
    done
    cp "$b" /data/ledger.db' sh "$quarantine" "$backup" </dev/null
  printf '%s\n' "$quarantine"
}
# start_release STATUS: поднять app и litestream текущего окружения и дождаться /health.
start_release() {
  dc up -d app litestream
  wait_health "$LEDGER_BUILD"
  save_current_release "$1"
}
