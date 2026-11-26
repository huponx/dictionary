# syntax=docker/dockerfile:1
# Build context is the repository root:
#   docker build -f build/dictionary-worker/Dockerfile -t dictionary-worker:dev .
ARG GO_VERSION=1.26

# cgo is on: the Anki library writes the deck with SQLite (go-sqlite3). The build therefore runs on the target
# platform (no --platform=$BUILDPLATFORM cross compile): a multi-arch image is built natively or under QEMU.
FROM golang:${GO_VERSION} AS build
WORKDIR /src
ENV CGO_ENABLED=1 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w" -o /out/dictionary-worker ./cmd/dictionary-worker

# glibc + CA certificates (HTTPS to the dictionary APIs), runs as non-root. The static image is not enough: the
# binary is dynamically linked against libc because of cgo.
FROM gcr.io/distroless/base-debian13:nonroot
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="dictionary-worker" \
      org.opencontainers.image.source="https://github.com/huponx/dictionary" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/dictionary-worker /dictionary-worker
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/dictionary-worker"]
