#!/usr/bin/env bash
# Run from this independent project, never from Terraform.
set -euo pipefail
umask 077
: "${IMAGE:?Set IMAGE to the full OCIR repository and release tag}"
OCI_INVENTORY_IMAGE="$IMAGE"
OCI_INVENTORY_REGISTRY="${IMAGE%%/*}"
OCI_INVENTORY_CONTEXT="$(cd "$(dirname "$0")/.." && pwd)"
[[ "$IMAGE" == */*:* && "$IMAGE" != *@* ]] || { echo "IMAGE must include registry, repository and release tag" >&2; exit 1; }
for task_tool in docker oci; do
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
# Uses the OCI CLI configuration/profile/environment selected by the operator.
# The short-lived bearer token stays in a private temporary Docker directory.
oci raw-request --http-method GET \
  --target-uri "https://${OCI_INVENTORY_REGISTRY}/20180419/docker/token" \
  --query data.token --raw-output |
  docker --config "$task_docker_config" login --username BEARER_TOKEN \
    --password-stdin "$OCI_INVENTORY_REGISTRY"
# Release tags are immutable by convention; publish new source with a new tag.
if docker --config "$task_docker_config" buildx imagetools inspect "$OCI_INVENTORY_IMAGE" >/dev/null 2>&1; then
  echo "Collector image already published: $OCI_INVENTORY_IMAGE"
  exit 0
fi
docker --config "$task_docker_config" buildx create --name "$task_builder" \
  --driver docker-container "$task_docker_host" >/dev/null
task_builder_created=true
docker --config "$task_docker_config" buildx build --builder "$task_builder" \
  --platform linux/amd64,linux/arm64 --provenance=false \
  --tag "$OCI_INVENTORY_IMAGE" --push "$OCI_INVENTORY_CONTEXT"
