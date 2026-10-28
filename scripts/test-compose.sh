#!/bin/sh
# Isolated mount/permission smoke test; never uses platform credentials or networks.
set -eu
image=${1:?usage: test-compose.sh IMAGE}
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
project="replybridge-test-$$"
volume="$project-data"
owner="$(id -u):$(id -g)"
compose() {
  docker compose --project-name "$project" --env-file "$work/compose.env" -f "$case_dir/compose.yaml" -f "$work/offline.yaml" "$@"
}
cleanup() {
  if [ -n "${case_dir:-}" ]; then compose down --remove-orphans >/dev/null 2>&1 || true; fi
  docker volume rm "$volume" >/dev/null 2>&1 || true
  docker run --rm --network none --user 0 --entrypoint /bin/sh --mount "type=bind,src=$work,dst=/test" "$image" -ec 'chown -R "$1" /test' sh "$owner" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
umask 077
cat > "$work/compose.env" <<EOF
SHADOWBRAIN_IMAGE=$image
SHADOWBRAIN_UID=10001
SHADOWBRAIN_GID=10001
SHADOWBRAIN_DATA_VOLUME=$volume
EOF
cat > "$work/offline.yaml" <<'EOF'
services:
  comment-bridge:
    network_mode: none
EOF
docker volume create "$volume" >/dev/null
for mode in bili youtube both; do
  case_dir="$work/$mode"
  mkdir -p "$case_dir/secrets" "$case_dir/bilibili-auth"
  cp "$root/deploy/comment-bridge/compose.yaml" "$case_dir/compose.yaml"
  printf '%s\n' 'test-only-not-a-secret' > "$case_dir/secrets/feishu-app-secret"
  youtube=false
  bili=false
  if [ "$mode" != bili ]; then
    youtube=true
    printf '%s\n' '{}' > "$case_dir/secrets/youtube-client.json"
    printf '%s\n' '{}' > "$case_dir/secrets/youtube-token.json"
  fi
  if [ "$mode" = both ]; then
    cp "$root/deploy/comment-bridge/compose.youtube-webhook.yaml" "$case_dir/compose.youtube-webhook.yaml"
    printf '%s\n' 'test-only-not-a-webhook' > "$case_dir/secrets/youtube-feishu-webhook"
  fi
  if [ "$mode" != youtube ]; then
    bili=true
    printf '%s\n' '{}' > "$case_dir/bilibili-auth/credential.json"
  fi
  printf 'SHADOWBRAIN_YOUTUBE_ENABLED=%s\nSHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED=%s\n' "$youtube" "$bili" > "$case_dir/runtime.env"
  docker run --rm --network none --user 0 --entrypoint /bin/sh --mount "type=bind,src=$case_dir,dst=/test" "$image" -ec 'chown -R 10001:10001 /test/secrets /test/bilibili-auth'
  compose config --quiet
  compose run --rm --no-deps data-init
  compose run --rm --no-deps comment-bridge version
  compose run --rm --no-deps --entrypoint /bin/sh comment-bridge -exc '
    test "$(id -u)" = 10001
    test -r /run/shadowbrain/secrets/feishu-app-secret
    test -w /var/lib/shadowbrain
    test "$(stat -c %a /var/lib/shadowbrain)" = 700
    if [ "$SHADOWBRAIN_YOUTUBE_ENABLED" = true ]; then
      test -r /run/shadowbrain/secrets/youtube-client.json
      test -r /run/shadowbrain/secrets/youtube-token.json
    else
      test ! -e /run/shadowbrain/secrets/youtube-client.json
    fi
    if [ "$SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED" = true ]; then
      test -r /run/shadowbrain/bilibili-auth/credential.json
      test -w /run/shadowbrain/bilibili-auth
    else
      test ! -e /run/shadowbrain/bilibili-auth/credential.json
    fi
  '
  if [ "$mode" = both ]; then
    docker compose --project-name "$project" --env-file "$work/compose.env" -f "$case_dir/compose.yaml" -f "$case_dir/compose.youtube-webhook.yaml" -f "$work/offline.yaml" run --rm --no-deps --entrypoint /bin/sh comment-bridge -ec 'test -r "$SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE"'
  fi
  printf '%s\n' "$mode: isolated Compose mounts OK"
done
