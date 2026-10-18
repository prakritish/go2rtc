# syntax=docker/dockerfile:1

# go2rtc with the record module, for production (NVR) use.
# Build from the repository root:
#   docker build -f docker/record/Dockerfile -t go2rtc-record .
#
# Differences from docker/Dockerfile: plain Alpine (no Python, ffplay, fonts, Intel drivers),
# non-root user, a separate /recordings volume and a health check.
# FFmpeg is included: it's needed for video.mp4?format=mp4 and &exact (remux, no transcoding)
# and for ffmpeg: sources (cameras with unsupported codecs, transcoding).

ARG GO_VERSION="1.25"
ARG ALPINE_VERSION="3.22"


# 1. Build go2rtc binary
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w" -trimpath -o /go2rtc


# 2. Final image
FROM alpine:${ALPINE_VERSION}

# tini - signal handling (go2rtc closes open segments on SIGTERM)
# tzdata - local time in logs and TZ support
RUN apk add --no-cache ffmpeg tini tzdata ca-certificates \
    && addgroup -S -g 1000 go2rtc \
    && adduser -S -D -H -u 1000 -G go2rtc go2rtc \
    && mkdir -p /config /recordings \
    && chown go2rtc:go2rtc /config /recordings

COPY --from=build /go2rtc /usr/local/bin/go2rtc

USER go2rtc
WORKDIR /config
VOLUME ["/config", "/recordings"]

# 1984 - WebUI and HTTP API, 8554 - RTSP, 8555 - WebRTC
EXPOSE 1984 8554 8555/tcp 8555/udp

# loopback requests skip API auth (unless api.local_auth is enabled)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:1984/api || exit 1

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["go2rtc", "-config", "/config/go2rtc.yaml"]
