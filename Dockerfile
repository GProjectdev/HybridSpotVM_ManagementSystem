FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMPONENT
RUN case "$COMPONENT" in vm-spot-risk-collector|policy-manager|checkpoint-coordinator|spot-recovery-controller|training-runtime-collector|spot-watcher) ;; *) echo "A supported COMPONENT build argument is required" >&2; exit 1 ;; esac \
    && CGO_ENABLED=0 GOOS=linux go build -mod=readonly -trimpath -o /manager "./cmd/$COMPONENT"
FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
