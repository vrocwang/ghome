# syntax=docker/dockerfile:1

# Build a static binary, then ship only that binary and a CA bundle.
#
# The runtime stage is `scratch`: about 7 MB, no shell, no package manager, no
# libc. Nothing to exploit and nothing to patch, which is what makes this
# comfortable to leave running on a small box.

# Declared before the first FROM so both stages see them.
#
# VERSION is what ghome reports from --version and in /healthz. It is passed in
# explicitly rather than inferred, so the image is correct no matter what the
# builder can or cannot read out of .git -- and unlike buildx's automatic git
# info, ghome actually prints this one.
#
#   docker build --build-arg VERSION=$(git describe --tags --always --dirty) .
#
# `make docker` does that for you.
ARG VERSION=dev
ARG VCS_REF=unknown

# --platform=$BUILDPLATFORM keeps the compiler native even when building for
# another architecture, so `docker buildx build --platform linux/amd64,linux/arm64`
# cross-compiles instead of emulating.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG VERSION
ARG VCS_REF

ARG TARGETOS
ARG TARGETARCH

# Installed rather than assumed. The runtime stage copies this exact file, so a
# missing one would break TLS against the forge at best, and the build at worst.
# `go mod download` needs it too.
RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# An empty GOOS/GOARCH means "host", so a plain `docker build` still works.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/ghome .

# A place for --cache-file, created here because scratch has no RUN to mkdir in.
RUN mkdir -p /out/data && chown 65534:65534 /out/data

FROM scratch

ARG VERSION
ARG VCS_REF

# The metadata BuildKit was trying to fill in when it warned. Set explicitly so
# the image is self-describing even though .git never enters the context.
LABEL org.opencontainers.image.title="ghome" \
      org.opencontainers.image.description="A navigation page whose content and styling live in git" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.source="https://git.xhupo.com/simon/ghome"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ghome /ghome
# Copied as a directory so it arrives owned by the unprivileged user; a fresh
# named volume mounted here inherits that ownership.
COPY --from=build --chown=65534:65534 /out/data /data

# Unprivileged, and there is no shell to reach anyway.
USER 65534:65534
WORKDIR /data

EXPOSE 8080

# Persisting the cache means a container that restarts while the forge is
# unreachable still serves the last good page. Mount a volume at /data.
#
# Note: passing any argument to `docker run` replaces this whole default, so
# repeat --listen and --cache-file if you override it.
CMD ["--listen=:8080", "--cache-file=/data/cache.yaml"]

# No curl and no shell here, so the binary probes itself. It exits non-zero on
# 503, which means "up but has no usable config yet" -- exactly the state a
# restart should fix, and one a bare port check would call healthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/ghome", "--healthcheck"]

ENTRYPOINT ["/ghome"]
