# syntax=docker/dockerfile:1.7
# Stages: deps, dev, test, lint, build, mock, runtime (default, last).

ARG GO_VERSION=1.27.1
ARG GOLANGCI_LINT_VERSION=v2.14.0
ARG AIR_VERSION=v1.67.4

# --- deps: module download, shared by every Go stage ------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS deps
WORKDIR /src
ENV GOFLAGS=-buildvcs=false
COPY go.mod go.sum ./
# Modules land in the image layer (offline test/lint/build stages reuse them);
# the cache mount keeps downloads across go.mod changes.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go mod download && go mod verify

# --- dev: hot reload with air, source bind-mounted by compose ---------------
FROM deps AS dev
ARG AIR_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/air-verse/air@${AIR_VERSION}
ENV CGO_ENABLED=0
EXPOSE 8080
CMD ["air", "-c", ".air.toml"]

# --- test: race detector needs cgo (this stage only) ------------------------
FROM deps AS test
ENV CGO_ENABLED=1
COPY . .
CMD ["go", "test", "-race", "-cover", "-count=1", "./..."]

# --- lint: official golangci-lint image + go vet ----------------------------
FROM golangci/golangci-lint:${GOLANGCI_LINT_VERSION} AS lint
WORKDIR /src
ENV GOFLAGS=-buildvcs=false
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY . .
CMD ["sh", "-c", "go vet ./... && golangci-lint run ./..."]

# --- build: static binaries, cross-compiled for the target platform ---------
FROM deps AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hangarr ./cmd/hangarr && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/mockarr ./cmd/mockarr && \
    mkdir -p /out/data

# --- mock: fake upstream services for dev and e2e ---------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS mock
COPY --from=build /out/mockarr /mockarr
COPY testdata /testdata
USER 65532:65532
EXPOSE 8989 7878 32400 8096 8181
ENTRYPOINT ["/mockarr"]

# --- runtime: what ships to Kubernetes --------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ARG VERSION=dev
LABEL org.opencontainers.image.title="hangarr" \
      org.opencontainers.image.description="Sonarr / Radarr watch statistics and cleanup, with Plex (+ Tautulli) or Jellyfin" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/hangarr /hangarr
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/hangarr", "healthcheck"]
ENTRYPOINT ["/hangarr"]
CMD ["serve"]
