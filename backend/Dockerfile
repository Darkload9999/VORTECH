# syntax=docker/dockerfile:1.7
#
# One image carries all backend binaries; the entrypoint selects the role:
#   api (default) | /app/worker | /app/migrate up
#
# Runtime image: distroless static, no shell, no package manager, runs as
# the unprivileged nonroot user (65532) and works with a read-only root FS.

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Dependency layer: cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w \
        -X github.com/Darkload9999/VORTECH/backend/internal/buildinfo.Version=${VERSION} \
        -X github.com/Darkload9999/VORTECH/backend/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/Darkload9999/VORTECH/backend/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="vortech-backend" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="Proprietary"

COPY --from=build /out/ /app/

USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/app/api"]
