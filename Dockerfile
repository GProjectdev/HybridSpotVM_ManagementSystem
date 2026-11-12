FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMPONENT
RUN case "$COMPONENT" in public-cloud-vm-collector|spot-vm-management-controller|checkpoint-recovery-coordinator|training-lifecycle-controller|training-runtime-controller|spot-watcher|placement-webhook) ;; *) echo "A supported v6 COMPONENT build argument is required" >&2; exit 1 ;; esac \
    && CGO_ENABLED=0 GOOS=linux go build -mod=readonly -trimpath -o /manager "./cmd/$COMPONENT"
FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
