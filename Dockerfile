# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Stages: deps, dev, test, lint, build, mock, runtime (default, last).
# Base images are pinned by digest (Dependabot proposes the updates). The Go
# version of the golang image must match the toolchain line of go.mod and the
# Compose images (make go-version-check).

ARG AIR_VERSION=v1.67.4

# --- deps: module download, shared by every Go stage ------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS deps
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
FROM golangci/golangci-lint:v2.15.0@sha256:fbfcfd12a6f8d7f1b97b3362ca80fc2a244d46863148c73cb88e7793af96c481 AS lint
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
    mkdir -p /out/data && \
    cp -r testdata /out/testdata && \
    rm -rf /out/testdata/captured

# --- mock: fake upstream services for dev and e2e ---------------------------
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88 AS mock
COPY --from=build /out/mockarr /mockarr
COPY --from=build /out/testdata /testdata
USER 65532:65532
EXPOSE 8989 7878 32400 8096 8181
ENTRYPOINT ["/mockarr"]

# --- runtime: what ships to Kubernetes --------------------------------------
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88 AS runtime
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
