# Changelog

## 4.9.2

**Fix MCS login: read server version byte; fix resource field format.**

The MCS server sends a version byte before the first response packet.
We were reading it as the first tag byte causing all subsequent reads
to be misaligned → `unexpected EOF`.

Also fixed the LoginRequest `id`/`device_id` fields to use the
`android-<016x>` hex format (padded 16 hex digits) that Google's
MCS server expects, and set `resource` to the FCM token when available.

## 4.9.1

**Fix FCM: MCS Run loop blocked on empty FCM token.**

The `Run` loop was guarding on `tok == ""` which caused it to spin-wait
forever when GCM registration fails (FIS blocked). Since MCS login only
requires `android_id` + `security_token`, the token check is removed.
MCS now connects immediately after checkin even without a GCM token.

## 4.9.0

**FCM: proceed to MCS without GCM token when FIS is blocked.**

Wyze's Firebase project blocks the FIS API for external callers, making GCM
token registration impossible. GCM registration is now best-effort — on
failure we warn and proceed to connect to MCS using the android_id/security_token
from the checkin step directly. The MCS connection itself does not require an
FCM token; it uses the AidLogin credentials. This gets us onto the MCS bus
where we can receive Wyze's ring push notifications as long as Wyze sends them
as topic/broadcast messages (which the Wyze app does for ring events).

## 4.8.9

**Fix FCM: use correct production Firebase credentials (project 738316279406).**

The `gcm_defaultSenderId` `332820210871` in resources.arsc was from a test/debug
Firebase config (`fir-test-d9f49`). The production Firebase App ID
`1:738316279406:android:3c34a4366b9bad6d` was also in resources.arsc but missed
in the initial extraction. Updated all FCM constants to use the production project:
- Sender ID: `738316279406`
- App ID: `1:738316279406:android:3c34a4366b9bad6d`
- Project ID: `738316279406`
FIS tokens are now requested against the correct production project.

## 4.8.8

**Fix FCM registration: INVALID_SENDER — try FIS with both API keys and production project ID.**

`INVALID_SENDER` on GCM register3 means the sender ID requires FIS auth,
but FIS was blocked on the test-project API key. The `gcm_defaultSenderId`
`332820210871` is the production GCP project number; added
`fisToken()` that tries both extracted API keys against both the test and
production project IDs, falling back to no-FIS if all are blocked.
The second API key (`AIzaSyDYCWq...`) may belong to the production project.

## 4.8.7

**Fix FCM registration: drop FIS token step (API_KEY_SERVICE_BLOCKED).**

The Firebase Installations API (`firebaseinstallations.googleapis.com`) is
blocked for Wyze's API key for external callers. Removed the FIS token step
entirely — the legacy GCM register3 endpoint accepts registrations with
just `AidLogin` authorization (android_id:security_token) without the
`X-Goog-Firebase-Installations-Auth` header when using a pre-Firebase-SDK-v17
flow, which is what the older Wyze app version uses.

## 4.8.6

**Fix FCM registration: FIS_AUTH_ERROR on GCM register.**

The GCM register endpoint requires a Firebase Installation Service (FIS)
auth token in `X-Goog-Firebase-Installations-Auth`, not the raw API key.
Added `fisToken()` which calls the FIS API first to get a short-lived
installation auth token, then uses it in the GCM register request.
Also drops the `x-android-cert` header (not required for non-Play-Integrity
flows) which was causing the auth failure.

## 4.8.5

**Expose FCM Push Listener toggle in HA add-on UI.**

- `home_assistant/wyze_bridge/config.yaml`: add `events.fcm: bool?` to schema
- `home_assistant/wyze_bridge/run.sh`: map `events.fcm` → `EVENTS_FCM`
- `home_assistant/wyze_bridge/translations/en.yaml`: label + description for the new toggle
- Also fixes duplicate `description:` key on `events.live_ring_dedupe_window` entry

## 4.8.4

**Add FCM push listener for real-time doorbell ring detection (`EVENTS_FCM=true`).**

Registers as a simulated Android device with Google's Firebase Cloud Messaging
service using credentials from the Wyze app. Maintains a persistent MCS
connection to receive the same push notifications the Wyze mobile app gets —
sub-second doorbell ring delivery, no polling, no TUTK required.

- `internal/fcm/fcm.go`: new FCM client — Android checkin, GCM token
  registration, MCS persistent TLS connection, DataMessageStanza parser,
  ring payload classifier.
- `internal/wyzeapi/commands.go`: `RegisterPushToken()` — registers the
  FCM device token with Wyze's `set_push_info` API so Wyze's backend
  delivers ring pushes to this device.
- `internal/config/config.go`: `EVENTS_FCM` env var (default `false`).
- `cmd/wyze-bridge/main.go`: `startFCMListener()` wired after live ring
  watcher; shares the same `RingDeduper` for cross-source deduplication.
- FCM registration state persisted to `$STATE_DIR/fcm_state.json`.

## 4.7.2

**Fix: boolean options (live_ring, record.all, filter.blocks, etc.) were silently ignored in the add-on UI.**

`bashio::config.has_value` treats `false` as "not set", which also caused
`true` values for options the user had never previously saved to not export.
Added `export_bool` helper in `run.sh` that reads directly from
`/data/options.json` via `jq`, bypassing the has_value check. All boolean
options now correctly export regardless of value:
`events.live_ring`, `bridge.auth`, `camera.audio`, `filter.blocks`,
`record.all`, `mqtt.enabled`, `gwell.enabled`, `debug.force_iotc_detail`.

## 4.7.1

**Expose live ring + diagnostics in HA add-on UI.**

- `home_assistant/wyze_bridge/config.yaml`: add `events.live_ring` (bool)
  and `events.live_ring_dedupe_window` (duration) to the add-on schema so
  the options appear in the Configuration tab.
- `home_assistant/wyze_bridge/run.sh`: map the new options to
  `EVENTS_LIVE_RING` / `EVENTS_LIVE_RING_DEDUPE_WINDOW`.
- `home_assistant/wyze_bridge/DOCS.md`: document the live ring feature,
  requirements, and minimal config example. Update the "one notification
  per session" note to reference the new per-press path.
- `home_assistant/wyze_bridge/translations/en.yaml`: labels + descriptions
  for the two new events fields so the UI shows human-readable help text.
- `internal/go2rtcmgr/apiclient.go`: log the full stream URL (including
  `&notify=true` when live ring is on) in the `AddStream` debug line.
- `cmd/wyze-bridge/main.go`: explicit debug log when live ring is disabled.

## 4.7.0

**Live per-press doorbell ring via TUTK IOCTRL control channel (experimental).**
Wyze's cloud groups rapid button presses into a single event, so you get
one HA notification per ring "session" regardless of how many times the
button was physically pressed. This release adds a local, real-time,
per-press alternative sourced directly from the camera's live control
channel — the same signal the Wyze app uses for its instant "someone is
at your door" alert.

- **`EVENTS_LIVE_RING=true`** enables the watcher. Off by default while
  experimental. Requires a TUTK-streamed doorbell (`HL_DB2` / `WYZEDB3`)
  and the forked go2rtc binary bundled in this release.
- **`EVENTS_LIVE_RING_DEDUPE_WINDOW`** (default `10s`): when the cloud
  poller is also enabled, the second arrival of the same physical press
  is suppressed within this window so HA fires exactly one event.
- go2rtc fork (`go2rtc/`): added `NotifyFunc` callback to `DTLSConn`
  in `pkg/tutk/dtls` to route unsolicited camera IOCTRL notifications
  to a caller-supplied handler without blocking the AV streaming loop.
  The wyze producer (`pkg/wyze`) installs this callback when
  `&notify=true` is in the stream URL, recognises the doorbell ring
  CommandID (10020 / `KCmdDoorbellRing`), and emits an unconditional
  `WYZE-NOTIFY {...}` JSON line on stdout. The bridge's `RingWatcher`
  in `internal/go2rtcmgr` intercepts that line before any log-level
  mapping and calls the shared `dispatchButtonPress` path.
- Both Dockerfiles and the HA add-on Dockerfile now compile go2rtc from
  the `go2rtc/` fork source instead of downloading the upstream binary.
  `USE_UPSTREAM_GO2RTC=1` reverts to the upstream prebuilt binary.
- All existing cloud-poller behavior (motion events, thumbnails) is
  unchanged when `EVENTS_LIVE_RING=false`.

## 4.6.8

**Polish: diagnostics + docs after confirming doorbell events and live
streaming work end-to-end.** No behavior change to the event pipeline —
button-press and motion continue to work as in 4.6.7.

- main.go: go2rtc's log level now follows the add-on's
  `debug.log_level`. Setting it to `debug`/`trace` finally surfaces
  go2rtc's own `wyze: dial ...` and TUTK/DTLS handshake lines, which are
  the only way to diagnose a stream `discovery timeout`. Previously only
  the obscure `force_iotc_detail` toggle did this; that flag still works
  as an override.
- events.go: stop re-processing an already-seen cloud event on every
  poll. Wyze keeps returning the same recent event (with a growing
  `end_time`) for a minute+, which produced ~40 duplicate log lines/sec
  at debug/trace for a single ring. The poller now advances its query
  window past seen events and only logs genuinely new ones. Dedupe /
  dispatch behavior is unchanged; added a regression test.
- DOCS: documented the two Wyze-side gotchas discovered in testing —
  (1) `discovery timeout` is caused by the Wyze app/Alexa holding the
  camera's single P2P session; close them (or make the bridge the sole
  consumer) to fix it; and (2) Wyze's cloud groups rapid button presses
  into one event, so you get one notification per ring "session," not
  per press. Added a Troubleshooting section.

## 4.6.7

**Fix: HA MQTT camera entity was blank.** The auto-discovered camera
entity pointed its image `topic` at the base topic where nothing is
published, so it never showed anything. (MQTT only ever carries a small
still JPEG — never video; live video is the RTSP/WebRTC stream.)

- discovery.go: point the MQTT camera at the `.../thumbnail` topic
  where the snapshot JPEG is actually published.
- main.go: capture one snapshot when a camera enters the streaming
  state, so the entity gets a fresh still even when SNAPSHOT_INTERVAL
  is disabled (previously it stayed blank until a manual/periodic snap).

Note: the MQTT camera entity is a periodically-refreshed STILL image.
For LIVE doorbell video, add a Generic Camera pointed at the bridge's
RTSP stream: `rtsp://<bridge-ip>:8554/<camera_name>` (still image:
`http://<bridge-ip>:5080/api/snapshot/<camera_name>`). See DOCS.

## 4.6.6

Diagnostics: one-time full raw-event JSON dump (first 10 events) at INFO
to capture exactly what a doorbell press sends over the cloud event API.

## 4.6.5

Event-pipeline diagnostics promoted to INFO so the drop reason is
visible without switching to debug. Each unique event now logs once at
INFO with its `event_value` / `event_tag_list` / classified kind / age,
plus an explicit line when it's skipped (stale window or untracked MAC)
or dispatched to the sinks. The per-poll "get_event_list returned
events" line is back to debug (it repeated the same event every 1.5s).

## 4.6.4

**Make local builds actually pick up new code + verifiable.** The
add-on Dockerfile clones the source in a Docker layer; without cache
busting, rebuilds silently reused a stale checkout — so earlier fixes
(nonce, classifier) may never have run on-device. Also, the version
was never passed to the build, so the log always said "dev".

- build.yaml now passes `VERSION` (shown at startup) and a `CACHE_BUST`
  arg that invalidates the git-clone layer each release, forcing a
  fresh checkout of `main` on every rebuild.
- Add an INFO "event poll heartbeat" (~every 60s) so operators can
  confirm from the default log level that the poller is calling the
  API and how many events it sees — silence is no longer ambiguous.
- `get_event_list returned events` now logs at INFO (was debug).

If your startup log still shows `"version":"dev"` after updating,
the rebuild did not pick up new source — uninstall/reinstall the
add-on or clear the build cache.

## 4.6.3

**Fix: no events were ever delivered.** The `get_event_list` call was
missing the `nonce` field that the v4 signed endpoint requires (its
sibling v4 call `get_streams` includes it). Without it the request was
rejected, so neither motion nor doorbell events ever arrived — and the
failure was logged only at debug level, so it was invisible.

- Add `nonce` to the `get_event_list` payload.
- Log a `get_event_list` failure at **warn** (not debug) so a broken
  event pipeline is visible at the default log level.
- Add debug logging of each received event's `event_id` /
  `event_value` / classified kind / age, and log when an event is
  dropped as stale or for an untracked MAC — so the pipeline can be
  diagnosed from the logs.
- Widen default `EVENT_RECENT_WINDOW` 30s → 120s (only relevant once
  events actually arrive).

## 4.6.2

**Fix doorbell button-press classification.** The event classifier
matched the wrong Wyze alarm type, so real doorbell rings were never
detected (they fell through to motion) and the HA `event` entity never
updated.

- Correct the ring signal to Wyze `EventAlarmType.DOORBELL_RANG` —
  `event_value == 10` (confirmed via shauntarves/wyze-sdk). The prior
  code wrongly used tag `13` (which is actually MOTION) and value `12`
  (which is FACE).
- Match the alarm type in the event's `event_value` field first, with
  `event_tag_list` as a fallback.
- Tests updated to the correct values (10=ring; 1/6/7/13=motion;
  12=face → motion).

## 4.6.1

Local-build fix for the HA add-on + doorbell dashboard card.

- **Fix add-on install (404):** the stable add-on pulled a prebuilt
  `ghcr.io/idisposable/...-homeassistant:<version>` image that didn't
  exist and wouldn't contain this fork's code. Removed `image:` and
  added `build.yaml`; the Dockerfile now compiles this fork's source
  (go2rtc + wyze-bridge + gwell-proxy) locally, so the add-on runs
  THIS repo's code.
- **Doorbell/motion events exposed in the add-on UI:** new `events`
  config section (`motion_api`, `recent_window`) mapped to
  `MOTION_API` / `EVENT_RECENT_WINDOW`.
- **Doorbell dashboard card:** the auto-generated Lovelace dashboard
  (`/dashboard.yaml`) now gives doorbell cameras a button-press
  entity on the picture-glance plus a dedicated "Last Ring / Live
  View" card. Ready-to-paste snippets added to DOCS.
- **Stable entity_id:** the button `event` entity now sets `object_id`
  so HA derives `event.wyze_<mac>_button` deterministically.
- DOCS.md documents the Doorbell Button-Press & Motion Events section.

## 4.6.0

**go2rtc customization** — three long-standing issues that all
wanted a piece of the same knob: give the operator control over
go2rtc's ports, its API auth, and the stream set it serves.

- **Basic auth on go2rtc's `/api/*`** ([#123](https://github.com/IDisposable/docker-wyze-bridge/issues/123)):
	set `GO2RTC_API_USERNAME` + `GO2RTC_API_PASSWORD` and the
	bridge configures go2rtc's `api.username/password`, forwards
	Basic auth on every internal API call, and proxies the same
	credentials through the WebUI's HLS + WebSocket paths so
	browsers don't see a login prompt.
- **Port overrides** ([#108](https://github.com/IDisposable/docker-wyze-bridge/issues/108)):
	`GO2RTC_API_PORT` / `GO2RTC_RTSP_PORT` / `GO2RTC_WEBRTC_PORT`
	move go2rtc off `:1984` / `:8554` / `:8889` when something
	else on the host wants them (e.g. Frigate on `:1984`).
- **Extra streams pass-through** ([#106](https://github.com/IDisposable/docker-wyze-bridge/issues/106)):
	`GO2RTC_EXTRA_STREAMS=name=source[,name=source]` registers
	additional streams alongside the Wyze cameras — RTSP, ONVIF,
	`ffmpeg:`, anything go2rtc understands. Camera names always
	win on collision.
- **Verbatim YAML escape hatch**: `GO2RTC_EXTRA_YAML` is appended
	to the managed config after a visible marker for anything the
	typed knobs above don't cover (custom `publish:` / `hass:` /
	ONVIF discovery blocks, etc.).
- HA add-on: new **go2rtc** options section wraps the same
	surface — `api_port`, `api_username`, `api_password`,
	`rtsp_port`, `webrtc_port`, `extra_streams`, `extra_yaml`.

Internal WebUI URL generation (RTSP-copy button, WebRTC copy) now
uses the configured ports instead of hard-coded 8554 / 1984.

**Doorbell button-press (ring) events.** Restores the cloud
event-polling path (dropped in the Go rewrite) and adds explicit
doorbell button-press detection. When a doorbell rings, the bridge
now surfaces it over MQTT, webhooks, and the `/metrics` event log.

- New env var `MOTION_API` (duration, e.g. `1500ms`; `0`/unset
  disables). Enables the `internal/events` poller that calls Wyze's
  `get_event_list`, dedupes by `event_id`, and classifies each event
  as motion or button-press via the event tag/value.
- New env var `EVENT_RECENT_WINDOW` (default `30s`) bounds how old an
  event may be and still be acted on, so a cold start doesn't replay a
  stale backlog.
- MQTT: doorbell cameras (`WYZEDB3`, `HL_DB2`, Doorbell Pro lineage)
  get a Home Assistant **`event` entity** (`event.<cam>_button`,
  `device_class: doorbell`, `event_types: [pressed]`) plus a **device
  trigger** so the ring is selectable in the HA automation UI as
  "<Nickname> — pressed".
- Button-press also fires a `button_press` webhook and records a
  `button_press` entry in the `/metrics` event log; motion events fire
  a `motion` webhook + momentary `…/motion` MQTT topic.
- New package `internal/events` with unit tests for classification,
  dedupe, stale-window filtering, and camera lookup.

## 4.5.0

Runtime **TUTK → WebRTC auto-fallback** for cameras Wyze crippled
via firmware. When a TUTK-path camera fails to stream 5 times in
a row (configurable via `TUTK_FALLBACK_THRESHOLD`), the bridge
promotes it to the WebRTC path for the rest of the process
lifetime — no operator intervention, no config edits, no restart.
Primarily targets `HL_CAM4` (Wyze V4) hit by the early-2025
firmware update but works for any model Wyze exposes over
mars-webcsrv.

- New env var `TUTK_FALLBACK_THRESHOLD` (default `5`, `0` disables).
- Per-camera `forceWebRTC` flag surfaces in `/api/metrics` as
	`protocol_forced: true` and on the metrics page as `webrtc
	(forced)`.
- Auto-promotion fires the issues registry entry
	`camera/fallback/<name>` (warn severity) so the change is
	visible in `/metrics`, `/api/health`, and
	`sensor.wyze_bridge_config_errors`.
- Manual `MODEL_OVERRIDES=HL_CAM4:is_webrtc=true` still works and
	takes precedence — operator intent wins over auto-fallback.
- Full design + testing notes in `DOCS/TUTK_WEBRTC_FALLBACK_DESIGN.md`.
- MIGRATION.md → Known Issues updated to reflect auto-recovery
	is now the default fix path.

## 4.4.2

Reliability + doc pass driven by open issue triage.

- **Stream auto-recovery** ([#100](https://github.com/IDisposable/docker-wyze-bridge/issues/100)):
	HealthCheck now routes dropped streams through the error/backoff
	path so the 10s reconnect ticker actually picks them up (was
	logging "reconnecting" without reconnecting). `connectCamera` also
	clears any prior go2rtc entry before re-registering so a stuck
	source pool gets fully torn down. Rename-in-Wyze-app orphans are
	reaped by MAC each discovery cycle instead of accumulating stale
	streams forever.
- **Grid audio stays muted** ([#112](https://github.com/IDisposable/docker-wyze-bridge/issues/112)):
	`<video-rtc>` now honors a `muted` attribute proactively, so the
	grid stays quiet even after the browser grants autoplay
	permission from a single-camera click. Detail page is unchanged.
- **Camera-name doc** ([#99](https://github.com/IDisposable/docker-wyze-bridge/issues/99)):
	Expanded MIGRATION.md → "Changed: Camera names" with the specific
	Frigate / HA `camera:` / Lovelace / Node-RED surfaces to update.
- **HL_CAM4 (V4) auto-hint** ([#117](https://github.com/IDisposable/docker-wyze-bridge/issues/117),
	[#92](https://github.com/IDisposable/docker-wyze-bridge/issues/92),
	[#87](https://github.com/IDisposable/docker-wyze-bridge/issues/87)):
	Wyze's early-2025 firmware disabled TUTK on newer V4 units. The
	issues registry now emits a targeted hint when it sees chronic
	TUTK timeouts on an `HL_CAM4` — the fix is a one-line override
	(`MODEL_OVERRIDES=HL_CAM4:is_webrtc=true`) documented in README +
	MIGRATION.md → "Known Issues". Runtime auto-fallback is designed
	for 4.5.

## 4.4.1

Hotfix for [#119](https://github.com/IDisposable/docker-wyze-bridge/issues/119):
OG cameras (`GW_GC1` / `GW_GC2`) worked on 4.3.0 but broke on 4.4.0
for many users. Field reports confirm Wyze's mars-webcsrv WebRTC
backend serves OG hardware reliably, so the default has flipped to
WebRTC. Users on 4.4.0 already applying the `is_webrtc=true` override
can drop it.

- **OG default is now WebRTC** (`GW_GC1`, `GW_GC2`) — no more
	gwell-proxy sidecar spawn for OG-only fleets.

## 4.4.0

Code-review pass + community PRs: hardening, parity, observability,
new camera models, and tests across the bridge.

### Camera support

- **AN_RDB1 (Doorbell Pro 2)** routed to the WebRTC path (was silently
	falling through to TUTK).
- **GW_DUO (Cam Pan Duo)** routed to the WebRTC path (mars-webcsrv
	signaling, same backend as the Doorbell Pro) with `is_pan` set.
- **GW_WC (Window Cam)** routed to Gwell P2P (LAN-direct, same path
	as OG cameras). Needs a manual LAN IP — see new HA UI below.
- **LD_CFP (Floodlight Pro)** routed to WebRTC via AWS KVS.
- **OG cameras (GW_GC1 / GC2)** now correctly classify as Gwell P2P
	even when the Wyze cloud reports an empty LAN IP.
- New `IsGwellP2P` registry flag distinguishes LAN-direct Gwell models
	(OG, Window Cam) from doorbell-lineage Gwell (Doorbell Pro / Duo).

### HA add-on UI

- **Manual LAN IPs** for Gwell cameras (under "Gwell Protocol
	Cameras → Manual LAN IPs"): list of `{mac, lan_ip}` entries that
	feed the new `GWELL_LAN_IPS` env var, applied at discovery time.
	Use this when the Wyze cloud doesn't report a LAN IP for `GW_DUO`
	or `GW_WC` and gwell-proxy can't lock LAN-direct.
- **Camera Model Registry overrides** (new section "Camera Model
	Registry → Model Overrides"): list of `{model, name, is_*}`
	entries that override or add rows in the bridge's model registry
	at startup via the new `MODEL_OVERRIDES` env var. Lets operators
	add a brand-new Wyze model code or flip routing flags on an
	existing one without rebuilding the bridge.

### Reliability

- **Pre-registered Gwell publish slots**: gwell-proxy's RTSP PUSH no
	longer races the runtime AddStream and gets dropped with a broken
	pipe; the slot is reserved in go2rtc's startup YAML.
- **gwell-proxy reconnect**: uses the cached P2P server endpoint on
	stream-error retries instead of re-running full discovery.
- **KVS signaling double-encode fix** (`FixKVSSignalingURL`): some
	Wyze cameras (observed on LD_CFP) return a SigV4-encoded URL with
	`%252F` instead of `%2F`; AWS KVS rejects with 403. Single-decode
	when the tell-tale `%25` is present.
- **WebRTC-streamer discovery branch**: `GetCameraList` skips the
	LAN-IP / P2P-ID checks for WebRTC-only models (LD_CFP doesn't
	report either) — MAC + Model is enough.
- Atomic state-file writes (write-to-temp + rename) under a write
	mutex; concurrent state-change goroutines no longer race the file.
- Wyze API auth-lifecycle observer wires failures + recoveries to the
	issues registry (`/metrics`, `/api/health`,
	`sensor.wyze_bridge_config_errors`).
- Chronic camera-error reporting: >10 consecutive failed connects on
	a camera posts a `camera/chronic/<name>` issue; cleared on next
	stream.
- MQTT publish backpressure: bounded waiter pool prevents goroutine
	leak when the broker is unreachable; loud-then-rate-limited drop
	log.
- Graceful ffmpeg recorder shutdown (SIGINT + `WaitDelay`) — last mp4
	segment finalizes cleanly instead of being SIGKILL-truncated.

### Architecture / quality

- Single `ModelSpec` registry replaces five hardcoded maps; adding a
	new camera is one row (or one Model Override entry above).
- `webui.NewServer` takes an `Options` struct (drops several Set*
	setters); `mqtt.NewClient` is ctx-first.
- `cmd/wyze-bridge/main.go`'s `wireCameraStateChanges` split into
	per-subsystem helpers.
- `issues.Registry` methods nil-safe (callers no longer guard).
- Typed `wyzeapi.GetCameraKVSConfig` replaces raw-map parsing.
- `/api/*` errors return JSON `{"error":"…"}` via `writeJSONError`.
- `mqtt.Client` + `webui.Server` propagate the bridge's signal-
	cancellable root context to fire-and-forget handler goroutines.
- Doorbell labels aligned with Wyze marketing names ("Wyze Video
	Doorbell Pro" / "Pro 2" / "Duo").
- `DOCS/GW_BE1_Research.md` captures pcap-based protocol notes.

### Observability & UX

- README "Issues registry" subsection explaining `config_errors`,
	active categories, and the three surfaces that show them.
- Actionable hints for known Wyze API response codes (1001 bad creds,
	1003 bad API key, 2001 token expired, 3019 MFA, …).
- Network-error classifier distinguishes DNS / timeout / `OpError`
	from generic transport failures; HTTP 5xx / 429 / 401-403 render
	with actionable text.
- `/metrics` page: per-section legend captions + hover tooltips on
	every column header and summary tile.
- DEVELOPER.md "Adding a new camera model" section.

### New env vars

- `GWELL_LAN_IPS=MAC=IP,MAC=IP` — pin LAN IPs for Gwell cameras the
	Wyze cloud doesn't report. HA UI feeds this from Gwell → Manual
	LAN IPs.
- `MODEL_OVERRIDES=MODEL:flag=v,flag=v;MODEL:...` — override or add
	model registry rows at startup. HA UI feeds this from Camera
	Model Registry → Model Overrides. Flags: `name`, `is_gwell`,
	`is_gwell_p2p`, `is_webrtc`, `is_pan`, `is_doorbell`.

### Tests

- Webui smoke coverage (prometheus, dashboard, metrics page+JSON,
	route table, HLS / WS proxy).
- `mqtt.Client` (constructor defaults, callback registration,
	`publishSem` saturation).
- `/api/*` actions (audio toggle, quality validation, record
	start/stop, `/api/discover` no-hook / GET-405 / with-hook, health
	degraded mode, KVS shim happy + reject).
- Model registry (`IsGwellP2P`, `IsWebRTCStreamer` matrix,
	`ApplyModelOverrides` parser, `FixKVSSignalingURL`,
	`gwellLanIPOverride`).
- New camera-classification tests for GW_WC, LD_CFP, GW_DUO.

### Docs

- `.env.dev.example` aligned with current env-var names.
- `DOCS/DESIGN.md` MQTT topic table synced.
- `DEVELOPER.md` release flow + Adding-a-camera-model.

### Credits

- PR #111 (Grady Neely): OG cameras (GW_GC1/GC2) classified as Gwell
	P2P even when the Wyze cloud returns an empty LAN IP.
- PR #116 (wlatic): GW_DUO + `GWELL_LAN_IPS` env var +
	pre-registered Gwell publish slots + gwell-proxy reconnect fix.
- PR #118 (Daniel Quick): GW_WC, LD_CFP, KVS double-encode fix,
	GW_DUO via WebRTC.

## 4.3.0

MQTT Phase 1 release focused on control parity improvements with the legacy bridge,
while documenting go2rtc-era control boundaries.

### Added

- MQTT stream control topics:
	- `{topic}/{cam}/state/set` (`start`/`stop`)
	- `{topic}/{cam}/power/set` (`on`/`off`/`restart`)
- MQTT power state publishing on `{topic}/{cam}/power`
- Cloud-backed MQTT property control mapping (write-only mirror):
	- `night_vision`, `irled`, `status_light`, `motion_detection`,
		`motion_tagging`, `bitrate`, `fps`, `hor_flip`, `ver_flip`
- Home Assistant discovery entities for Phase 1 controls:
	- stream switch, power switch, reboot button, snapshot button
	- IR/status light/motion detection/motion tagging switches
	- bitrate/fps number controls
	- horizontal/vertical flip switches
- MQTT capability and scope reference in `DOCS/MQTT_SPEC.md`

### Changed

- Night vision mapping aligned to cloud property semantics (`auto` => `3`)
- MQTT expectations now explicitly split into `Implemented`, `Phase 1`, and `Deferred`
	in the spec to clarify what requires direct TUTK control vs. cloud API fallback

### Notes

- Live camera property readback parity from the Python bridge remains deferred because
	go2rtc owns the active TUTK session and does not expose a Wyze property-control API.
- Pan/tilt K110xx command parity and full `{prop}/get` MQTT readback are out of scope
	for this phase.



## 4.2.2

Complete rewrite in Go. See [MIGRATION.md](https://github.com/IDisposable/docker-wyze-bridge/blob/main/MIGRATION.md) for upgrade instructions.

### Added

- go2rtc-based streaming (pure Go TUTK P2P, no binary SDK)
- MFA/TOTP login support
- Server-Sent Events for real-time WebUI updates
- Structured JSON logging via zerolog
- Recording with configurable path templates and auto-pruning
- Sunrise/sunset snapshot scheduling
- Metrics endpoints

### Changed

- Docker image under 25 MB (was 200+ MB)
- WebUI completely redesigned (dark theme, grid layout, WebRTC player)
- State persistence via JSON (replaces pickle files)
- MQTT auto-detects Home Assistant Mosquitto broker

### Removed

- Python runtime and all Python dependencies
- Binary TUTK SDK (`.so` files)
- MediaMTX (replaced by go2rtc)
- FFmpeg
- Remote P2P streaming (LAN only; use VPN for remote access)
- `ON_DEMAND` setting (all cameras connect eagerly)
- `MTX_*` environment variables
- Per-camera STREAM_AUTH (global credentials only)
- Unraid template
