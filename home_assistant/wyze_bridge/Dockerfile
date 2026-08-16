# Self-contained local build for the Home Assistant add-on.
#
# Unlike the CI image (which copies a pre-built binary from
# WYZE_BRIDGE_IMAGE), this Dockerfile compiles the wyze-bridge +
# gwell-proxy Go binaries and fetches go2rtc entirely on its own, so
# `ha addon` local builds work without any pre-published image. The
# source is cloned from this fork at BUILD_REF, so the add-on always
# runs THIS repo's code — including the doorbell button-press feature.
#
# Override BUILD_REPO / BUILD_REF to build a different fork or branch.

ARG BUILD_FROM=ghcr.io/home-assistant/base:3.21
ARG GO2RTC_VERSION=1.9.14
ARG BUILD_REPO=https://github.com/Xzatrox/docker-wyze-bridge
ARG BUILD_REF=main
ARG VERSION=dev

# ── Stage 1: fetch go2rtc binary + video-rtc.js ──────────────────────────────
FROM alpine:3.21 AS go2rtc-fetch
ARG GO2RTC_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates && \
    ARCH=${TARGETARCH}; \
    curl -fsSL \
      "https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_${ARCH}" \
      -o /go2rtc && \
    chmod +x /go2rtc && \
    curl -fsSL \
      "https://github.com/AlexxIT/go2rtc/raw/v${GO2RTC_VERSION}/www/video-rtc.js" \
      -o /video-rtc.js

# ── Stage 2: compile the Go binaries from this fork's source ─────────────────
FROM golang:1.26.4-alpine AS builder
ARG BUILD_REPO
ARG BUILD_REF
ARG VERSION
# CACHE_BUST changes per release so the git clone layer below is not
# served from Docker's cache with a stale checkout. Referencing it in
# an env right before the clone invalidates everything from here down
# whenever it changes.
ARG CACHE_BUST=dev
RUN apk add --no-cache git ca-certificates
WORKDIR /src
RUN echo "cache-bust: ${CACHE_BUST}" && \
    git clone --depth 1 --branch "${BUILD_REF}" "${BUILD_REPO}" . && \
    go mod download
# Replace placeholder video-rtc.js with the real one before it's embedded.
COPY --from=go2rtc-fetch /video-rtc.js ./internal/webui/static/video-rtc.js
RUN CGO_ENABLED=0 GOOS=linux \
      go build -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /wyze-bridge ./cmd/wyze-bridge && \
    CGO_ENABLED=0 GOOS=linux \
      go build -ldflags="-s -w" \
      -o /gwell-proxy ./cmd/gwell-proxy

# ── Stage 3: HA add-on runtime ───────────────────────────────────────────────
FROM ${BUILD_FROM}

# ffmpeg is needed by go2rtc's JPEG snapshot endpoint + the gwell-proxy
# sidecar. ca-certificates + tzdata for Wyze API TLS and timestamps.
RUN apk add --no-cache ca-certificates tzdata ffmpeg && \
    mkdir -p /config /media/wyze_bridge/snapshots /media/wyze_bridge/recordings

COPY --from=builder      /wyze-bridge /usr/local/bin/wyze-bridge
COPY --from=builder      /gwell-proxy /usr/local/bin/gwell-proxy
COPY --from=go2rtc-fetch /go2rtc      /usr/local/bin/go2rtc

COPY run.sh /
RUN chmod +x /run.sh

CMD [ "/run.sh" ]
