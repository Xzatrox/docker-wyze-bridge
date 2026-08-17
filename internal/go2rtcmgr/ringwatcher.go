package go2rtcmgr

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ringNotifyPrefix is the unconditional stdout marker emitted by the
// forked go2rtc wyze source when it receives a doorbell ring IOCTRL
// notification on the live control channel.
const ringNotifyPrefix = "WYZE-NOTIFY "

// ioctrlDumpPrefix is the diagnostic dump emitted for every unsolicited
// IOCTRL frame during Phase 0 capture. The bridge logs it at INFO so the
// real ring CommandID can be identified from a live press.
const ioctrlDumpPrefix = "WYZE-IOCTRL "

// RingEvent is a per-press doorbell ring observed on the live TUTK
// control channel (not the cloud poller). One of these is synthesized
// each time the forked go2rtc prints a WYZE-NOTIFY line.
type RingEvent struct {
	// Stream is the go2rtc stream name (normalized camera name).
	Stream string
	// MAC is the device MAC address, uppercase and colonless.
	MAC string
	// TS is the camera-reported timestamp, or the receipt time when
	// the camera omits one.
	TS time.Time
}

// ringNotifyPayload is the JSON shape emitted by the forked go2rtc.
// Example: {"stream":"front_doorbell","mac":"80482C2F8BDF","kind":"ring","ts":1786912182903}
type ringNotifyPayload struct {
	Stream string `json:"stream"`
	MAC    string `json:"mac"`
	Kind   string `json:"kind"`
	// TS is epoch-milliseconds. Optional — falls back to receipt time.
	TS int64 `json:"ts"`
}

// RingWatcher decodes ring notifications emitted by the forked go2rtc
// wyze source and invokes OnRing for each confirmed press.
//
// Wire HandleLogLine into Manager.emitLogLine BEFORE any level mapping
// so the WYZE-NOTIFY line is consumed and never forwarded to zerolog.
type RingWatcher struct {
	// OnRing is called synchronously in the go2rtc stdout goroutine for
	// each valid ring line. Implementations must be goroutine-safe and
	// should not block for long.
	OnRing func(RingEvent)
	log    zerolog.Logger
}

// NewRingWatcher creates a RingWatcher with the given logger.
func NewRingWatcher(log zerolog.Logger) *RingWatcher {
	return &RingWatcher{log: log}
}

// HandleLogLine inspects one go2rtc stdout line. If it is a
// WYZE-NOTIFY ring line, it parses the JSON payload and calls OnRing.
// If it is a WYZE-IOCTRL diagnostic line, it logs it at INFO for
// Phase 0 CommandID capture.
// Returns true when the line was consumed (the caller should skip its
// normal level mapping). Returns false for all other lines.
func (w *RingWatcher) HandleLogLine(line string) (consumed bool) {
	// Phase 0 diagnostic dump — log every unsolicited IOCTRL frame at
	// INFO so the real ring CommandID is visible in the bridge log.
	if strings.HasPrefix(line, ioctrlDumpPrefix) {
		w.log.Info().Str("raw", line).Msg("TUTK unsolicited IOCTRL frame (Phase 0 capture)")
		return true
	}

	if !strings.HasPrefix(line, ringNotifyPrefix) {
		return false
	}

	raw := strings.TrimPrefix(line, ringNotifyPrefix)
	var p ringNotifyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		// Malformed JSON: consumed (don't let it bleed into level
		// mapping as a misleading ERR line), but don't call OnRing.
		w.log.Warn().Err(err).Str("raw", raw).Msg("malformed WYZE-NOTIFY payload")
		return true
	}

	if p.Kind != "ring" {
		// A future firmware may emit other notification kinds on the
		// same prefix; skip gracefully.
		w.log.Debug().Str("kind", p.Kind).Msg("WYZE-NOTIFY: ignoring non-ring notification")
		return true
	}

	ts := time.Now()
	if p.TS > 0 {
		ts = time.UnixMilli(p.TS)
	}

	ev := RingEvent{
		Stream: p.Stream,
		MAC:    p.MAC,
		TS:     ts,
	}

	w.log.Info().
		Str("stream", ev.Stream).
		Str("mac", ev.MAC).
		Time("ts", ev.TS).
		Msg("live ring received from TUTK control channel")

	if w.OnRing != nil {
		w.OnRing(ev)
	}
	return true
}

// RingDeduper suppresses duplicate button-press events within a
// configurable window. Designed for the live-ring + cloud-poller case
// where a single physical press can arrive from both sources.
//
// Rule: if a press for the same camera arrives within dedupeWindow of
// the most recent press (from any source), it is suppressed. The first
// press to arrive is the one that fires; subsequent arrivals within the
// window are deduplicated.
type RingDeduper struct {
	mu         sync.Mutex
	window     time.Duration
	lastRingAt map[string]time.Time // keyed by camera name
}

// NewRingDeduper creates a RingDeduper with the given deduplication window.
func NewRingDeduper(window time.Duration) *RingDeduper {
	return &RingDeduper{
		window:     window,
		lastRingAt: make(map[string]time.Time),
	}
}

// ShouldDispatch returns true when the press for camName should be
// dispatched (i.e. it is NOT a duplicate within the window). It also
// records the press time so subsequent calls within the window return
// false. Thread-safe; called from both the cloud-poller goroutine and
// the go2rtc-stdout goroutine concurrently.
func (d *RingDeduper) ShouldDispatch(camName string, at time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	last, seen := d.lastRingAt[camName]
	if seen && at.Sub(last) < d.window {
		return false
	}
	d.lastRingAt[camName] = at
	return true
}
