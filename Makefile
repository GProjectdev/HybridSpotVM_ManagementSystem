GO ?= go
COMPONENT ?=
REGISTRY ?= ghcr.io/gprojectdev
TAG ?= dev
IMG ?= $(REGISTRY)/$(COMPONENT):$(TAG)
.PHONY: test build image push
test:
	$(GO) test -mod=readonly ./...
build:
	$(GO) build -mod=readonly ./cmd/...
image:
	@test -n "$(COMPONENT)" || (echo "Set COMPONENT, e.g. policy-manager"; exit 1)
	docker build --build-arg COMPONENT=$(COMPONENT) -t $(IMG) .
push:
	@test -n "$(COMPONENT)" || (echo "Set COMPONENT, e.g. policy-manager"; exit 1)
	docker push $(IMG)
