GO ?= go
IMG ?= ghcr.io/gprojectdev/hybridspot-management:dev
.PHONY: test build image
test:
	$(GO) test -mod=readonly ./...
build:
	$(GO) build -mod=readonly ./cmd/manager
image:
	docker build -t $(IMG) .
