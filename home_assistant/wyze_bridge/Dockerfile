# Self-contained local build for the Home Assistant add-on.
#
# The go2rtc fork source lives at go2rtc/ inside this repo — no network
# fetches needed for it. Everything is built from the cloned source.
# Setting USE_UPSTREAM_GO2RTC=1 falls back to the upstream prebuilt binary.
#
# Override BUILD_REPO / BUILD_REF to build a different fork or branch.

ARG BUILD_FROM=ghcr.io/home-assistant/base:3.21
ARG GO2RTC_VERSION=1.9.14
ARG BUILD_REPO=https://github.com/Xzatrox/docker-wyze-bridge
ARG BUILD_REF=main
ARG VERSION=dev
ARG GO2RTC_FORK_REF=wyze-ring
ARG USE_UPSTREAM_GO2RTC=0

# ── Stage 1: compile all Go binaries from this fork's source ─────────────────
FROM golang:1.26.4-alpine AS builder
ARG BUILD_REPO
ARG BUILD_REF
ARG VERSION
ARG GO2RTC_VERSION
ARG GO2RTC_FORK_REF
ARG USE_UPSTREAM_GO2RTC
ARG TARGETARCH
# CACHE_BUST changes per release so the git clone layer below is not
# served from Docker's cache with a stale checkout.
ARG CACHE_BUST=dev
RUN apk add --no-cache git ca-certificates curl
WORKDIR /src
RUN echo "cache-bust: ${CACHE_BUST}" && \
    git clone --depth 1 --branch "${BUILD_REF}" "${BUILD_REPO}" . && \
    go mod download

# Build go2rtc from the fork source vendored at go2rtc/ in this repo.
# video-rtc.js is also in the repo at go2rtc/www/video-rtc.js — no
# network fetch needed. USE_UPSTREAM_GO2RTC=1 downloads a prebuilt binary
# instead, as a fallback if the fork build breaks on a given arch.
RUN echo "go2rtc build (ref: ${GO2RTC_FORK_REF})" && \
    if [ "${USE_UPSTREAM_GO2RTC}" = "1" ]; then \
      echo "USE_UPSTREAM_GO2RTC=1: downloading upstream release binary"; \
      ARCH=${TARGETARCH}; \
      curl -fsSL \
        "https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_${ARCH}" \
        -o /go2rtc && \
      chmod +x /go2rtc; \
    else \
      cd /src/go2rtc && \
      go mod download && \
      CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /go2rtc . && \
      echo "go2rtc fork build complete"; \
    fi

# video-rtc.js is already in the repo at go2rtc/www/video-rtc.js
RUN cp /src/go2rtc/www/video-rtc.js /src/internal/webui/static/video-rtc.js

# Build wyze-bridge and gwell-proxy
WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux \
      go build -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /wyze-bridge ./cmd/wyze-bridge && \
    CGO_ENABLED=0 GOOS=linux \
      go build -ldflags="-s -w" \
      -o /gwell-proxy ./cmd/gwell-proxy

# ── Stage 2: HA add-on runtime ───────────────────────────────────────────────
FROM ${BUILD_FROM}

# ffmpeg is needed by go2rtc's JPEG snapshot endpoint + the gwell-proxy
# sidecar. ca-certificates + tzdata for Wyze API TLS and timestamps.
RUN apk add --no-cache ca-certificates tzdata ffmpeg && \
    mkdir -p /config /media/wyze_bridge/snapshots /media/wyze_bridge/recordings

COPY --from=builder /wyze-bridge /usr/local/bin/wyze-bridge
COPY --from=builder /gwell-proxy /usr/local/bin/gwell-proxy
COPY --from=builder /go2rtc      /usr/local/bin/go2rtc

# run.sh lives inside the cloned source at home_assistant/wyze_bridge/run.sh
COPY --from=builder /src/home_assistant/wyze_bridge/run.sh /run.sh
RUN chmod +x /run.sh

CMD [ "/run.sh" ]
