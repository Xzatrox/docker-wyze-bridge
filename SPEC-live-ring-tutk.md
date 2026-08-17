# Specification: True Per-Press Doorbell Ring via the Live TUTK Control Channel

**Status:** Proposed
**Target repo:** `github.com/IDisposable/docker-wyze-bridge` (fork: `Xzatrox/docker-wyze-bridge`)
**Author:** engineering notes
**Related version baseline:** 4.6.8
**Scope:** Add an un-throttled, per-press doorbell ring signal sourced from the camera's live TUTK IOCTRL control channel, and surface it through the existing event pipeline (MQTT `event` entity, webhooks, `/metrics` event log) alongside the current cloud-poll ring/motion events.

---

## 1. Background & Motivation

### 1.1 What works today (4.6.8)

- A cloud event poller (`internal/events/events.go`) calls Wyze's
  `get_event_list` on an interval, classifies each event as `motion`
  (`event_value` in {1,6,7,13}) or `button_press` (`event_value == 10`,
  `DOORBELL_RANG`), dedupes by `event_id`, and dispatches via a `Sink`
  (`OnButtonPress` / `OnMotion`) to MQTT + webhooks + the `/metrics`
  event log.
- Live streaming is handled by the **go2rtc** sidecar via its native
  `wyze://` source (TUTK P2P + DTLS), added over the go2rtc HTTP API by
  `internal/go2rtcmgr`.

### 1.2 The limitation this spec addresses

The cloud path has a hard ceiling: **Wyze's cloud groups multiple rapid
button presses into a single event record**. It keeps returning the same
`event_id` with a growing `event_resources.end_time` for the entire
recording window (observed ~20–30s), so several physical presses collapse
into **one** `button_press` dispatch — one HA notification, one Alexa
routine, regardless of how many times the button was pressed. This was
confirmed empirically (see `CHANGELOG.md` 4.6.8 and the topic history):
event `...101786909575` fired once; subsequent presses within the window
never produced a new `event_id`.

The **only** un-throttled, per-press ring signal is delivered on the
camera's **live TUTK IOCTRL control channel** — the same channel the
Wyze app listens on to pop its instant "someone is at your door" alert.
That signal is:

- **Real-time** (sub-second, not the ~1.5–8s cloud latency).
- **Per-press** (the camera emits one control notification per physical
  press; no cloud-side de-duplication window).
- **Local** (no dependency on Wyze cloud availability or throttling).

### 1.3 Why it isn't captured today

go2rtc's `internal/wyze` (a.k.a. `pkg/wyze`) producer is a **media
producer**. After the TUTK/DTLS/AV-login/K-auth handshake it demuxes
**video and audio** AV frames into go2rtc tracks. Non-media IOCTRL
frames that the camera pushes on the control channel (including the ring
notification) are not surfaced to consumers — the producer effectively
drops anything that isn't an AV media frame. There is no go2rtc API to
subscribe to raw control-channel notifications, so the ring never leaves
the go2rtc process.

Capturing it therefore requires **forking / extending go2rtc's `wyze`
source** to (a) keep the control channel readable after streaming
starts, (b) recognize the ring notification IOCTRL frame, and (c) expose
it to the bridge process.

---

## 2. Goal & Non-Goals

### 2.1 Goal

Deliver a **per-press, low-latency doorbell ring event** to the existing
`events.Sink.OnButtonPress` path, sourced from the live TUTK control
channel, for doorbell models that stream via the go2rtc `wyze://` source
(primary target: **HL_DB2**, Wyze Video Doorbell v2; also WYZEDB3).

The new signal must:

1. Fire once per physical press (no cloud-style grouping).
2. Reuse the current downstream fan-out unchanged (MQTT `event` entity,
   `device_automation` trigger, webhooks, `/metrics` log).
3. Coexist with the cloud poller (which still provides motion and a
   thumbnail URL) without producing duplicate HA "pressed" events.
4. Degrade gracefully: if the fork/feature is unavailable or the control
   channel is not connected, behavior falls back to today's cloud-only
   ring with no regressions.

### 2.2 Non-Goals

- Replacing the cloud event poller. Motion events and event thumbnails
  continue to come from the cloud path.
- Two-way audio, PTZ, or other control-channel commands. This spec only
  *reads* the ring notification (and, optionally, other unsolicited
  status notifications) — it does not add outbound control features.
- Supporting Gwell/IoTVideo doorbells (GW_BE1, GW_DBD, AN_RDB1). Those
  use a different protocol stack (`internal/gwell`) and are out of scope
  here; a follow-up may add an analogous hook there.
- Bundling/redistributing a modified go2rtc binary in this repo's normal
  release channel beyond what Section 7 describes.

---

## 3. Protocol Background (grounding for implementers)

Sourced from the `seydx/tutk_wyze` reverse-engineering reference and the
`kroo/wyzecam` `tutk_protocol` command docs. HL_DB2 on FW 4.51.x uses the
**OLD (TransCode) protocol with DTLS** (`dtls=true`), confirmed by the
bridge's discovery log (`dtls:true`, `fw:4.51.3.6791`).

### 3.1 Session establishment (already done by go2rtc)

```
IOTC discovery (0x0601/0x0602) → session (0x0402/0x0404)
  → DTLS 1.2 handshake (PSK = SHA256(ENR), TLS_ECDHE_PSK_WITH_CHACHA20_POLY1305)
  → AV login (magic 0x0000 / 0x2000 → 0x2100)
  → K-auth: K10000 → K10001(challenge) → K10002/K10008(response) → K10003/K10009
  → streaming begins
```

### 3.2 The control channel (IOCTRL)

K-commands ride inside **IOCTRL frames** multiplexed onto the same DTLS
session as AV media. Structure (see `seydx/tutk_wyze` §7):

- **IOCTRL frame wrapper** (`IOCTRLMagic = 0x7000`, `IOType = 0x0100`),
  carrying an **HL header** (`"HL"` magic, version 5, 2-byte little-endian
  `CommandID`, 2-byte `PayloadLen`) followed by a command-specific payload
  (frequently JSON).

- Commands are a **mux**: the client sends commands and the camera sends
  responses *and* **unsolicited notifications** on the same channel
  (`kroo/wyzecam` `tutk_ioctl_mux`). The doorbell ring is one of these
  unsolicited camera→client notifications.

### 3.3 The ring notification

- The ring is a **camera-initiated IOCTRL notification** in the K-command
  space (a `1xxxx` command the camera pushes without the client asking).
  It is distinct from the AV video/audio frame path.
- **Action item (must be confirmed during Spike, Section 9):** the exact
  `CommandID` for the HL_DB2 ring notification and its payload shape must
  be captured from a live session. Candidate references to verify
  against: the mrlt8 Python bridge's TUTK notification handling and the
  `kroo/wyzecam` command table (notification-range K-commands). Do **not**
  hard-code a guessed code; derive it from a real capture (Section 9.1).
- Working hypothesis to validate: the camera emits a specific notify
  K-command carrying a small JSON or single-byte "doorbell/ring" event
  code at the instant of the press. It may also emit related notifications
  (e.g., PIR/motion). The classifier must match the **ring** notification
  specifically and ignore the rest.

> Rationale for the Spike-first approach: firmware 4.51.x is the same
> "security improvements" family that changed handshake behavior; the
> notification opcode/format for HL_DB2 on current firmware must be
> observed, not assumed.

---

## 4. Design Overview

Three components, from camera to Home Assistant:

```
┌────────────────────────────────────────────────────────────────┐
│ go2rtc (forked wyze source)                                     │
│  - keeps IOCTRL control channel readable during streaming       │
│  - recognizes the ring notification K-command                   │
│  - emits a structured "ring" signal out-of-band from AV tracks  │
│         │                                                        │
│         ▼  (transport: see Section 5)                            │
├────────────────────────────────────────────────────────────────┤
│ bridge: internal/go2rtcmgr (new: RingWatcher)                   │
│  - subscribes to go2rtc's ring signal for each doorbell stream  │
│  - normalizes into events.Event{Kind: button_press}            │
│         │                                                        │
│         ▼                                                        │
├────────────────────────────────────────────────────────────────┤
│ bridge: events.Sink.OnButtonPress (UNCHANGED downstream)        │
│  - MQTT event entity + device trigger                           │
│  - webhook button_press                                          │
│  - /metrics event log                                            │
│  - (NEW) cross-source dedupe vs cloud poller                     │
└────────────────────────────────────────────────────────────────┘
```

Key design principle: **the fork's only job is to get the ring out of the
go2rtc process.** All product logic (entities, dedupe, webhooks) stays in
the bridge, reusing the 4.6.8 pipeline.

---

## 5. go2rtc Fork: Options for Exposing the Ring

The forked go2rtc `wyze` source must emit the ring to the bridge. Three
transport options, in recommended order:

### Option A — go2rtc WebSocket API event (RECOMMENDED)

Extend the fork to publish a JSON message on go2rtc's existing API layer
(e.g., a new WS topic like `wyze/notify` or reuse the API event stream)
carrying `{stream, mac, kind:"ring", ts}`. The bridge already talks to
go2rtc's HTTP API on `:1984` (`internal/go2rtcmgr/apiclient.go`), so
adding a WS subscription is a natural extension and requires no extra
ports or files.

- **Pros:** in-band with existing go2rtc API; survives stream
  add/remove; no filesystem/log parsing; structured payload.
- **Cons:** requires touching go2rtc's API server code in the fork.

### Option B — Structured stdout log line (LOWEST FORK EFFORT)

Have the fork print a single, greppable, structured line to stdout when
the ring fires, e.g.:

```
WYZE-NOTIFY {"stream":"front_doorbell","mac":"80482C2F8BDF","kind":"ring","ts":1786912182903}
```

The bridge already ingests go2rtc stdout line-by-line
(`internal/go2rtcmgr/manager.go` `emitLogLine`) and classifies levels.
Add a parser that recognizes the `WYZE-NOTIFY` prefix and routes it to the
RingWatcher **before** the generic level mapping.

- **Pros:** minimal fork surface (one `fmt.Println`); reuses the existing
  stdout pipe; trivial to implement and test.
- **Cons:** log-channel coupling; must guarantee the line is emitted even
  when go2rtc log level is `warn` (print unconditionally, not via the
  logger); brittle if go2rtc changes stdout handling.

### Option C — Sidecar UDP/unix-socket emitter

Fork emits a datagram to a local socket the bridge listens on.

- **Pros:** clean separation from logs/API.
- **Cons:** most moving parts (new socket lifecycle, port/permission
  management); not justified given A/B exist.

**Decision:** Implement **Option B first** (fastest path to a working
end-to-end per-press ring and easiest to test), with **Option A** as the
follow-up hardening once the notification format is proven. Keep the
bridge-side abstraction (`RingWatcher`, Section 6) transport-agnostic so
switching B→A does not touch the event pipeline.

---

## 6. Bridge-Side Changes

### 6.1 New: `internal/go2rtcmgr` RingWatcher

A component that turns the go2rtc ring transport (Option B line, later
Option A WS) into calls on a callback.

```go
// RingEvent is a per-press doorbell ring observed on the live control
// channel (not the cloud poller).
type RingEvent struct {
    Stream string    // go2rtc stream name (normalized camera name)
    MAC    string    // device MAC, uppercase, colonless
    TS     time.Time // camera-reported or receipt timestamp
}

// RingWatcher decodes ring notifications emitted by the forked go2rtc
// wyze source and invokes OnRing for each press.
type RingWatcher struct {
    OnRing func(RingEvent)
    log    zerolog.Logger
}

// HandleLogLine (Option B) inspects one go2rtc stdout line and, if it is
// a WYZE-NOTIFY ring line, parses it and calls OnRing. Returns true if
// the line was consumed (so the manager can skip normal level mapping).
func (w *RingWatcher) HandleLogLine(line string) (consumed bool)
```

- Wire `HandleLogLine` into `Manager.emitLogLine` **before**
  `shouldSuppressGo2RTCLogLine` / level mapping in
  `internal/go2rtcmgr/manager.go`.
- The manager already has a `zerolog.Logger`; pass a child logger with
  `c=wyze-ring`.

### 6.2 `cmd/wyze-bridge/main.go` wiring

- Construct a `RingWatcher` whose `OnRing` resolves the stream/MAC to a
  camera name (reuse the existing `lookup`/`camMgr` used by the poller)
  and invokes the **same** `events.Event{Kind: button_press}` handling
  the poller uses.
- To avoid duplicating the `OnButtonPress` body, refactor the closure in
  `main.go` (lines ~452–468, the `OnButtonPress` func) into a named
  method/func `dispatchButtonPress(ev events.Event, camName string)` and
  call it from **both** the poller sink and the RingWatcher.
- `RingEvent` → `events.Event`: set `ID` to a synthesized id
  (`"ring:" + mac + ":" + ts`), `TS`, and leave `Thumbnail` empty (the
  live channel has no thumbnail; see dedupe note 6.4 about enriching from
  cloud).

### 6.3 Config additions (`internal/config` + add-on schema)

Add an `events` sub-option to gate the feature (default **off** while
experimental):

- `events.live_ring` (bool, default `false`) — enable the live TUTK ring
  watcher. Env: `EVENTS_LIVE_RING`.
- `events.live_ring_dedupe_window` (duration, default `10s`) — window for
  cross-source dedupe against the cloud poller (Section 6.4). Env:
  `EVENTS_LIVE_RING_DEDUPE_WINDOW`.

Mirror in:
- `internal/config/config.go` (`env`/`envBool`/`envDuration`, struct
  fields, defaults).
- `home_assistant/wyze_bridge/config.yaml` (`options.events` +
  `schema.events`).
- `home_assistant/wyze_bridge/run.sh` (`export_opt 'events.live_ring'
  EVENTS_LIVE_RING` etc.).
- `home_assistant/wyze_bridge/DOCS.md` (events section).

The live ring requires the camera's **live stream/control channel to be
connected**. Since the bridge already keeps doorbell streams connected
(the stream is added to go2rtc at discovery), no new "keepalive" is
required *if* the control channel stays open while a producer exists. If
go2rtc tears down the producer when there are no consumers, the fork must
keep the control channel alive independently, or the bridge must hold a
lightweight consumer open for doorbells (see Open Question 11.2).

### 6.4 Cross-source dedupe (critical)

With `live_ring` on, a single press can be seen **twice**: once by the
live watcher (fast) and once by the cloud poller (seconds later, same
physical press). Home Assistant must fire **one** `pressed` event.

Rule:
- Maintain a per-camera `lastRingAt time.Time` updated by **whichever
  source fires first**.
- When either source produces a button press, if
  `now - lastRingAt < live_ring_dedupe_window`, **suppress** the second
  (log at debug: `suppressed duplicate ring cross-source`).
- Live source is preferred as the "primary" (it is faster and per-press);
  the cloud event within the window is treated as the duplicate. However,
  the cloud event carries a **thumbnail URL** the live event lacks — if a
  downstream consumer wants the thumbnail, the dispatch may optionally be
  *enriched*: fire on the live edge immediately, then update the same
  logical event's thumbnail when the matching cloud event arrives (nice-
  to-have; see Non-Goals — default behavior is simple suppression).
- Because rapid multi-presses are exactly the case the cloud collapses,
  the dedupe window must be **short** (default 10s) so that genuinely
  separate live presses spaced >window apart are NOT suppressed. This is
  the whole point of the feature; do not set the window as wide as the
  cloud's ~30s grouping window.

Implementation location: a small `ringDeduper` used inside
`dispatchButtonPress`, keyed by camera name, guarded by a mutex (poller
goroutine and ring watcher goroutine both call it).

### 6.5 Downstream: unchanged

`mqtt.PublishButtonPress`, `webhooks.EventButtonPress`, and the
`webui.EventLog` recording remain exactly as in 4.6.8. The HA `event`
entity (`device_class: doorbell`, `event_types: ["pressed"]`) and the
`device_automation` trigger already exist and need no change.

---

## 7. go2rtc Fork Management & Build

### 7.1 Fork location

- Fork `AlexxIT/go2rtc` (the version pinned in the add-on:
  `GO2RTC_VERSION` in `home_assistant/wyze_bridge/build.yaml`, currently
  `1.9.14`) under the `Xzatrox` org, e.g.
  `github.com/Xzatrox/go2rtc`, branch `wyze-ring`.
- Base the change on the exact pinned tag so behavior is otherwise
  identical to today's stream (which is confirmed working).

### 7.2 Fork changes (minimal, Option B)

In `internal/wyze` of the go2rtc fork:
1. Keep reading the DTLS/IOCTRL channel after streaming starts (do not
   discard non-AV frames).
2. Demux IOCTRL frames: parse the HL header; on the confirmed ring
   `CommandID`, emit the structured `WYZE-NOTIFY {...}` stdout line
   (Option B) — `fmt.Println`, unconditional, not via go2rtc's leveled
   logger (so it survives `log.level: warn`).
3. Include the stream name and MAC (both are known to the producer from
   the `wyze://...&mac=...` URL) so the bridge can resolve the camera.

Guard the whole thing behind a query flag on the source URL, e.g.
`wyze://...&notify=true`, so the fork is a strict superset of upstream and
is inert unless the bridge opts in. The bridge sets `notify=true` on the
`wyze://` URL only for doorbell models when `events.live_ring` is on
(URL is built in `internal/wyzeapi/models.go` `StreamURL`).

### 7.3 Building the fork into the add-on

`home_assistant/wyze_bridge/Dockerfile` currently downloads a prebuilt
go2rtc release binary:

```
https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_${ARCH}
```

Change the build to compile the fork from source instead:
- Add a Go build stage that `git clone --branch wyze-ring
  https://github.com/Xzatrox/go2rtc` and `go build` for the target
  `${ARCH}` (amd64, aarch64), producing the `go2rtc` binary copied into
  the final image.
- Add a `GO2RTC_FORK_REF` build arg (commit SHA/tag) and include it in the
  `CACHE_BUST` chain so a fork bump forces a rebuild (mirrors the existing
  `CACHE_BUST`/`VERSION` handling that fixed stale `git clone` layers).
- Keep the ability to fall back to the upstream release binary via a build
  arg (`USE_UPSTREAM_GO2RTC=1`) so the stream still works if the fork
  build breaks.

### 7.4 Native/dev build

`DEVELOPER.md` documents downloading `go2rtc` v1.9.14 to the repo root.
Add a note: to develop the live ring, build the fork and place its binary
at `./go2rtc` (the bridge auto-detects `./go2rtc`/`./go2rtc.exe`).

---

## 8. Data Flow (end to end)

1. Discovery adds the doorbell stream to go2rtc with
   `wyze://<ip>?...&dtls=true&notify=true` (notify only when
   `events.live_ring=true` and model is a doorbell).
2. go2rtc (fork) completes TUTK/DTLS/AV/K-auth, streams video/audio, and
   **also** watches IOCTRL notifications.
3. User presses the button → camera pushes the ring IOCTRL notification →
   fork emits `WYZE-NOTIFY {"stream":...,"mac":...,"kind":"ring","ts":...}`
   on stdout.
4. `Manager.emitLogLine` hands the line to `RingWatcher.HandleLogLine`,
   which parses it and calls `OnRing`.
5. `OnRing` resolves camera name and calls `dispatchButtonPress`.
6. `dispatchButtonPress` runs cross-source dedupe; if not a duplicate,
   fans out to MQTT (`event` entity fires `pressed`), webhook, and the
   `/metrics` log — same as the cloud path.
7. Seconds later the cloud poller may report the same press; the deduper
   suppresses it.

Latency target: **< 1s** from press to MQTT publish (vs 1.5–8s cloud).

---

## 9. Implementation Plan (phased)

### Phase 0 — Spike: capture the ring notification (BLOCKING)

Goal: obtain the exact IOCTRL `CommandID` + payload for HL_DB2 ring on
current firmware. This de-risks everything else.

- 9.1 Instrument a local build of the fork to dump **all** IOCTRL frames
  (HL CommandID + hex payload) to stdout during a live session.
- 9.2 Press the physical button several times; correlate timestamps to
  identify the ring notification frame(s). Distinguish ring from motion/PIR
  notifications.
- 9.3 Record: CommandID, payload layout, whether payload disambiguates
  single vs multi press, and whether the camera also emits a matching
  cloud event (it does — for thumbnail enrichment/dedupe).
- **Exit criteria:** a documented, reproducible ring frame signature.
- **Fallback if no per-press notification exists on this firmware:** stop;
  document that the live channel does not expose a distinct per-press ring
  on HL_DB2 4.51.x, and the cloud limitation is irreducible. (This is the
  honest kill-switch — the feature is only worth building if the Spike
  confirms the signal exists.)

### Phase 1 — go2rtc fork: emit ring (Option B)

- 9.4 Fork at pinned tag; add `notify=true` gated IOCTRL watcher.
- 9.5 Emit `WYZE-NOTIFY {...}` on the confirmed frame.
- 9.6 Manual test: run fork locally, `wyze://...&notify=true`, press
  button, observe stdout line.

### Phase 2 — bridge: RingWatcher + wiring + dedupe

- 9.7 Add `RingWatcher` (`internal/go2rtcmgr`), hook into `emitLogLine`.
- 9.8 Refactor `OnButtonPress` closure → shared `dispatchButtonPress`.
- 9.9 Add `ringDeduper` (cross-source, mutex-guarded).
- 9.10 Add config (`events.live_ring`, `events.live_ring_dedupe_window`)
  across `config.go`, `config.yaml`, `run.sh`, `DOCS.md`.
- 9.11 Set `notify=true` on `StreamURL` for doorbells when enabled.

### Phase 3 — build & release

- 9.12 Dockerfile: compile fork from source; `GO2RTC_FORK_REF` +
  `CACHE_BUST`; `USE_UPSTREAM_GO2RTC` fallback.
- 9.13 Version bump + CHANGELOG; update DOCS troubleshooting.

### Phase 4 — validation

- 9.14 End-to-end: single press → one HA `pressed`, <1s.
- 9.15 Rapid multi-press → **N** HA events (the whole point), not one.
- 9.16 Cloud+live both on → exactly one event per press (dedupe works).
- 9.17 Feature off → identical to 4.6.8 (no regression).
- 9.18 Stream health unaffected (video still plays).

---

## 10. Testing Strategy

Codebase is test-heavy; match existing patterns
(`internal/events/events_test.go`, `internal/go2rtcmgr/*_test.go`).

### 10.1 Unit tests (bridge)

- `RingWatcher.HandleLogLine`:
  - valid `WYZE-NOTIFY` line → parsed `RingEvent`, `consumed=true`,
    `OnRing` called once.
  - non-notify go2rtc line → `consumed=false`, `OnRing` not called.
  - malformed JSON after prefix → `consumed=true` (don't leak to level
    mapping), `OnRing` not called, warn logged.
- `ringDeduper`:
  - two sources within window → one dispatch.
  - two presses spaced > window → two dispatches.
  - concurrent calls (poller + watcher goroutines) → race-free
    (`go test -race`).
- `dispatchButtonPress`: called from both paths, fans out identically.
- `StreamURL`: `notify=true` present only for doorbell + enabled;
  absent otherwise (extend `internal/wyzeapi/models_test.go`).
- Config parsing: `EVENTS_LIVE_RING`, `EVENTS_LIVE_RING_DEDUPE_WINDOW`
  defaults + overrides (`internal/config`).

### 10.2 go2rtc fork tests

- Table test over captured IOCTRL frames (from Phase 0): ring frame →
  emits notify; non-ring frames (motion/PIR/keepalive) → no emit.
- Ensure `notify=false`/absent → zero behavior change vs upstream
  (produces identical AV tracks).

### 10.3 Manual / integration

- The Phase 4 checklist (9.14–9.18). Capture add-on logs at
  `debug.log_level=debug` (now propagated to go2rtc as of 4.6.8) to
  observe both the fork's notify line and the bridge's dispatch.

---

## 11. Risks & Open Questions

1. **(Spike-blocking)** Does HL_DB2 4.51.x actually emit a distinct
   per-press ring notification on the live IOCTRL channel, separate from
   the throttled cloud event? Phase 0 must confirm; if not, the feature
   is not achievable and the effort stops there.
2. **Control-channel liveness without consumers.** go2rtc may stop the
   `wyze` producer when no consumer is attached. If so, the ring channel
   is dead when nobody is viewing. Options: (a) fork keeps the producer/
   control channel alive when `notify=true`; (b) bridge holds a
   minimal-bitrate consumer open for doorbells. Decide during Phase 1.
   Note the single-session constraint (a live producer means the Wyze
   app/Alexa cannot also hold the camera — see 4.6.8 DOCS troubleshooting).
3. **Maintenance burden of a go2rtc fork.** Every go2rtc bump must be
   rebased. Mitigate by keeping the diff tiny (Option B: one watcher +
   one print) and behind `notify=true`. Consider upstreaming the notify
   hook to `AlexxIT/go2rtc` so the fork can be retired.
4. **Firmware drift.** Wyze may change the notification opcode/format in a
   future FW. The classifier must fail safe (unknown frame → ignore, never
   crash the stream). Add a debug dump toggle to re-capture if it breaks.
5. **DTLS/handshake regressions in the fork.** Building from source at the
   pinned tag should preserve today's working stream; the
   `USE_UPSTREAM_GO2RTC` fallback protects releases if the fork build
   regresses streaming.
6. **Cross-source ordering.** If the cloud event ever beats the live one
   (unlikely but possible under load), dedupe keys on time window, not on
   source order, so it still collapses to one event.
7. **Protocol variant.** HL_DB2 4.51.x is OLD/TransCode+DTLS. If a future
   doorbell uses the NEW (0xCC51/HMAC-SHA1) protocol, the IOCTRL layer is
   the same above DTLS, but confirm during the Spike if targeting other
   models.

---

## 12. Acceptance Criteria

- With `events.live_ring=true` on a HL_DB2:
  - A single physical press produces exactly one HA `pressed` event in
    **< 1s** (median), verified via `event.<camera>_button` and the
    `/metrics` log.
  - **Rapid repeated presses each produce their own HA event** — the core
    improvement over the cloud path (which yields one).
  - With the cloud poller also enabled, each physical press still results
    in exactly one HA event (no live+cloud duplicates).
- With `events.live_ring=false` (default): behavior is byte-for-byte the
  4.6.8 experience; no new log noise; stream unaffected.
- `go build ./...` clean; `go test ./... -race` green for all touched
  packages (pre-existing Windows-only `internal/recording/
  TestBuildFFmpegArgs` path-separator failures excepted — they pass in
  Linux CI).
- Add-on builds with the forked go2rtc; startup log shows the new version;
  `USE_UPSTREAM_GO2RTC=1` still yields a working stream.

---

## 13. Affected Files (summary)

**go2rtc fork (`github.com/Xzatrox/go2rtc`, branch `wyze-ring`):**
- `internal/wyze/*` — IOCTRL notify watcher + `WYZE-NOTIFY` emit, gated on
  `notify=true`.

**this repo (`docker-wyze-bridge`):**
- `internal/go2rtcmgr/manager.go` — hook `RingWatcher.HandleLogLine` into
  `emitLogLine`.
- `internal/go2rtcmgr/ringwatcher.go` (new) — `RingWatcher`, `RingEvent`.
- `cmd/wyze-bridge/main.go` — refactor `OnButtonPress` → shared
  `dispatchButtonPress`; construct + wire `RingWatcher`; `ringDeduper`.
- `internal/wyzeapi/models.go` — `notify=true` on `StreamURL` for
  doorbells when enabled.
- `internal/config/config.go` — `EVENTS_LIVE_RING`,
  `EVENTS_LIVE_RING_DEDUPE_WINDOW`.
- `home_assistant/wyze_bridge/{config.yaml,run.sh,DOCS.md,Dockerfile,build.yaml,CHANGELOG.md}`
  — schema/env mapping, docs, fork build, version bump.
- Tests: `internal/go2rtcmgr/ringwatcher_test.go` (new),
  `internal/wyzeapi/models_test.go`, `internal/config/config_test.go`,
  and dedupe tests.

---

## 14. Rollout

- Ship behind `events.live_ring=false` by default (experimental).
- Document as "experimental: real-time per-press ring (requires forked
  go2rtc built into this add-on)".
- Once validated on HL_DB2 across a firmware cycle, consider enabling by
  default for doorbells and upstreaming the go2rtc notify hook.
