.DEFAULT_GOAL := help
.PHONY: help test build image publish
PLATFORMS ?= linux/amd64,linux/arm64
VERSION ?= v0.1.0
IMAGE ?= ghcr.io/architects-that-code/oci-compute-inventory:$(VERSION)

help:
	@printf '%s\n' \
	  'Usage: make <target> [VERSION=<release>] [IMAGE=<registry/repository:tag>]' \
	  '' \
	  'Targets:' \
	  '  help     Show this help (default)' \
	  '  test     Run Go race tests, vet, and dashboard validation' \
	  '  build    Test and build the native static binary in dist/collector' \
	  '  image    Test and build a local OCI image archive' \
	  '  publish  Test, build, and push to GitHub Container Registry' \
	  '' \
	  'Options:' \
	  '  VERSION    Release tag (default: v0.1.0)' \
	  '  IMAGE      Override ghcr.io/architects-that-code/oci-compute-inventory:VERSION' \
	  '  PLATFORMS  Local image architectures (default: linux/amd64,linux/arm64)' \
	  '             publish always builds both architectures' \
	  '' \
	  'Example: make publish VERSION=v0.1.0'

test:
	cd collector && go test -race ./... && go vet ./...
	python3 scripts/validate-dashboard.py

build: test
	mkdir -p dist
	cd collector && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../dist/collector .

image: test
	@test -n "$(IMAGE)" || (echo 'Set IMAGE to a release image reference' >&2; exit 1)
	mkdir -p dist
	docker buildx build --platform $(PLATFORMS) --provenance=false --tag "$(IMAGE)" --output type=oci,dest=dist/collector.oci.tar .

publish: test
	IMAGE="$(IMAGE)" bash scripts/publish-image.sh
