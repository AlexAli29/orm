# The orm CLI as an image, for people who would rather not install Go.
#
# Two targets, because the commands genuinely differ in what they need:
#
#   runtime    (default, the last stage) migrate, sqlmigrate, inspect — a static binary on
#              distroless. Nothing but the CLI and the CA certificates a TLS
#              connection to PostgreSQL needs.
#
#   toolchain  check, generate, makemigrations — the same binary on the Go
#              image. Those three read your entity source through
#              golang.org/x/tools/go/packages, which runs `go list`, so they
#              need a Go toolchain and a module that resolves. An image without
#              one would fail at the point of use rather than at the point of
#              choosing, which is the wrong end.
#
# Build both:
#   docker build -t orm:latest .                          # runtime
#   docker build -t orm:toolchain --target toolchain .    # with Go
#
# runtime is the last stage deliberately: `docker build` with no --target builds
# the final one, so the default has to be the image most people want.

# ---- build ------------------------------------------------------------------

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

# The module is downloaded before the source is copied, so editing code does not
# re-download the world.
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETOS and TARGETARCH are set by buildx. CGO is off so the result is static
# and runs on an image with no libc at all.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/orm ./cmd/orm

# ---- toolchain --------------------------------------------------------------

FROM golang:1.24-alpine AS toolchain

# git, because `go list` fetches modules for a project whose dependencies are
# not vendored.
RUN apk add --no-cache git ca-certificates

COPY --from=build /out/orm /usr/local/bin/orm

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/orm"]
CMD ["version"]

# ---- runtime ----------------------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# distroless static brings the CA bundle and a nonroot user, and brings no shell.
# A migration runner does not need one, and its absence is worth having in an
# image that holds a database URL.
COPY --from=build /out/orm /usr/local/bin/orm

WORKDIR /work
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/orm"]
CMD ["version"]
