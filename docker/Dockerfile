# ── Stage 1: Build go2rtc from local fork source ────────────────────────────
# go2rtc/ is vendored in this repo — no network fetch needed.
# video-rtc.js is also in the repo at go2rtc/www/video-rtc.js.
FROM golang:1.26.4-alpine AS go2rtc-build
ARG GO2RTC_VERSION=1.9.14
ARG GO2RTC_FORK_REF=wyze-ring
ARG USE_UPSTREAM_GO2RTC=0
ARG TARGETARCH
ARG CACHE_BUST=dev
RUN apk add --no-cache git ca-certificates curl
WORKDIR /go2rtc-src
COPY go2rtc/ .
RUN echo "go2rtc fork build (ref: ${GO2RTC_FORK_REF}, bust: ${CACHE_BUST})" && \
    if [ "${USE_UPSTREAM_GO2RTC}" = "1" ]; then \
      echo "USE_UPSTREAM_GO2RTC=1: falling back to upstream release binary"; \
      ARCH=${TARGETARCH}; \
      if [ "$ARCH" = "arm" ]; then ARCH="arm"; fi; \
      curl -fsSL \
        "https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_${ARCH}" \
        -o /go2rtc && \
      chmod +x /go2rtc; \
    else \
      go mod download && \
      CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /go2rtc . && \
      echo "go2rtc fork build complete"; \
    fi
# video-rtc.js is already in the repo — copy from local source
RUN cp /go2rtc-src/www/video-rtc.js /video-rtc.js

# ── Stage 2: Build Go binary ──────────────────────────────────────────────────
FROM golang:1.26.4-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Replace placeholder video-rtc.js with real one before embedding
COPY --from=go2rtc-build /video-rtc.js ./internal/webui/static/video-rtc.js
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /wyze-bridge ./cmd/wyze-bridge

# gwell-proxy — sidecar for OG Gwell cameras (GW_GC1/GC2) that speak
# Wyze's Gwell/IoTVideo P2P protocol on the LAN. Doorbell-lineage GW_*
# models (GW_BE1 Doorbell Pro, GW_DBD Doorbell Duo) use WebRTC instead,
# handled by go2rtc's native `#format=wyze` source type — no sidecar
# there, just a shim handler in internal/webui/shim_kvs.go that feeds
# go2rtc the Wyze KVS signaling URL + ICE servers.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" \
    -o /gwell-proxy ./cmd/gwell-proxy

# ── Stage 3: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21
# ffmpeg is used by two things in this image:
#   1. go2rtc's /api/frame.jpeg snapshot endpoint (JPEG frame extraction)
#   2. the optional gwell-proxy sidecar, which spawns an ffmpeg subprocess
#      per GW_* camera to RTSP-PUSH H.264 into go2rtc's RTSP server.
# Alpine's ffmpeg package adds ~40 MB, bringing the total image to ~60–65 MB
# (still well under the 200+ MB Python bridge). If you have no GW_* cameras
# AND don't need JPEG snapshots, you could omit ffmpeg and everything else
# (RTSP/HLS/WebRTC playback) keeps working.
RUN apk add --no-cache ca-certificates tzdata ffmpeg && \
    mkdir -p /config /media/snapshots /media/recordings

COPY --from=builder      /wyze-bridge   /usr/local/bin/wyze-bridge
COPY --from=builder      /gwell-proxy   /usr/local/bin/gwell-proxy
COPY --from=go2rtc-build /go2rtc        /usr/local/bin/go2rtc

EXPOSE 5080
EXPOSE 1984
EXPOSE 8554
EXPOSE 8888
EXPOSE 8889
EXPOSE 8189/udp

VOLUME ["/config", "/media"]

ENTRYPOINT ["/usr/local/bin/wyze-bridge"]
