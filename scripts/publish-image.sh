#!/usr/bin/env bash
# Build and publish this collector project.
set -euo pipefail
umask 077
: "${IMAGE:?Set IMAGE to the full GHCR repository and release tag}"
INVENTORY_IMAGE="$IMAGE"
INVENTORY_REGISTRY="${IMAGE%%/*}"
INVENTORY_CONTEXT="$(cd "$(dirname "$0")/.." && pwd)"
[[ "$IMAGE" == ghcr.io/*:* && "$IMAGE" != *@* ]] || { echo "IMAGE must include ghcr.io, repository and release tag" >&2; exit 1; }
for task_tool in docker; do
  command -v "$task_tool" >/dev/null || { echo "$task_tool is required on the build runner" >&2; exit 1; }
done
# Resolve the caller's Docker endpoint before isolating registry credentials.
task_docker_host="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
docker version --format '{{.Server.Version}}' >/dev/null
docker buildx version >/dev/null
task_buildx_path="$(docker info --format '{{range .ClientInfo.Plugins}}{{if eq .Name "buildx"}}{{.Path}}{{end}}{{end}}')"
[[ -x "$task_buildx_path" ]] || { echo "Docker Buildx executable not found" >&2; exit 1; }
task_docker_config="$(mktemp -d)"
mkdir -p "$task_docker_config/cli-plugins"
# Preserve plugin discovery (including Homebrew installs) without copying auth.
ln -s "$task_buildx_path" "$task_docker_config/cli-plugins/docker-buildx"
task_builder="oci-inventory-${RANDOM}-$$"
task_builder_created=false
cleanup() {
  if [[ "$task_builder_created" == true ]]; then
    docker --config "$task_docker_config" buildx rm --force "$task_builder" >/dev/null 2>&1 || true
  fi
  rm -rf "$task_docker_config"
}
trap cleanup EXIT
export DOCKER_HOST="$task_docker_host"
# Use an explicit package token, Actions token, or the signed-in GitHub CLI.
# Tokens travel over stdin and exist only in the temporary Docker configuration.
task_token="${GHCR_TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -z "$task_token" ]]; then
  command -v gh >/dev/null || { echo "Set GHCR_TOKEN and GHCR_USERNAME, or sign in with gh auth login" >&2; exit 1; }
  task_token="$(gh auth token)"
  task_scopes="$(gh api --include user | tr -d '\r' | sed -n 's/^[Xx]-[Oo][Aa]uth-[Ss]copes: *//p')"
  if [[ ",${task_scopes// /}," != *,write:packages,* ]]; then
    echo 'GitHub CLI token lacks write:packages; a public repository still requires package push permission.' >&2
    echo 'Use the Publish container GitHub Actions workflow, or run gh auth refresh -h github.com -s write:packages and retry.' >&2
    exit 1
  fi
fi
task_username="${GHCR_USERNAME:-${GITHUB_ACTOR:-}}"
if [[ -z "$task_username" ]]; then
  command -v gh >/dev/null || { echo "Set GHCR_USERNAME to the token owner's GitHub username" >&2; exit 1; }
  task_username="$(gh api user --jq .login)"
fi
printf '%s' "$task_token" | docker --config "$task_docker_config" login \
  --username "$task_username" --password-stdin "$INVENTORY_REGISTRY"
unset task_token
# Release tags are immutable by convention; publish new source with a new tag.
if docker --config "$task_docker_config" buildx imagetools inspect "$INVENTORY_IMAGE" >/dev/null 2>&1; then
  echo "Collector image already published: $INVENTORY_IMAGE"
  exit 0
fi
docker --config "$task_docker_config" buildx create --name "$task_builder" \
  --driver docker-container "$task_docker_host" >/dev/null
task_builder_created=true
docker --config "$task_docker_config" buildx build --builder "$task_builder" \
  --platform linux/amd64,linux/arm64 --provenance=false \
  --tag "$INVENTORY_IMAGE" --push "$INVENTORY_CONTEXT"
