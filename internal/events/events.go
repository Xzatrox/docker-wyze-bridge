// Package events polls the Wyze cloud event API and classifies each
// event as motion or a doorbell button-press (ring), then dispatches
// them to the bridge's sinks (MQTT, webhooks, /metrics event log).
//
// Background: the go2rtc-based rewrite dropped the cloud event-polling
// path that the older Python bridge had. Wyze delivers both motion and
// doorbell-ring notifications through the same get_event_list API; the
// event's tag/value distinguishes them. This package restores that
// polling loop and adds explicit button-press classification.
package events

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// Kind is the classified type of a Wyze cloud event.
type Kind int

const (
	// KindMotion is a normal motion / AI-detection event.
	KindMotion Kind = iota
	// KindButtonPress is a doorbell ring (button pressed).
	KindButtonPress
)

func (k Kind) String() string {
	switch k {
	case KindButtonPress:
		return "button_press"
	default:
		return "motion"
	}
}

// Wyze EventAlarmType values (from com.HLApi.Obj.EventItem, confirmed
// via shauntarves/wyze-sdk EventAlarmType). The doorbell button-press
// ("ring") is alarm type 10 (DOORBELL_RANG). For reference, the other
// types are: SOUND=2, OTHER=3, SMOKE=4, CO=5, DOORBELL_RANG=10,
// SCENE=11, FACE=12, and MOTION = {1, 6, 7, 13}. The alarm type is
// carried in the event's `event_value` field.
const wyzeDoorbellRangAlarmType = "10"

// Event is a normalized, classified Wyze cloud event.
type Event struct {
	ID       string // event_id (dedupe key)
	MAC      string // device_id / device_mac
	Kind     Kind
	TS       time.Time // event_ts converted to local time
	Thumbnail string   // first file_list url of type image, if any
}

// classify inspects a raw get_event_list entry and returns whether it
// is a doorbell button-press. The alarm type lives in `event_value`;
// DOORBELL_RANG (10) is a ring, everything else (motion=1/6/7/13,
// sound=2, face=12, …) is treated as motion by the caller.
func classify(raw map[string]interface{}) Kind {
	if tagMatches(raw["event_value"], wyzeDoorbellRangAlarmType) {
		return KindButtonPress
	}
	// Some firmware also echoes the alarm type in event_tag_list.
	if tags, ok := raw["event_tag_list"].([]interface{}); ok {
		for _, t := range tags {
			if tagMatches(t, wyzeDoorbellRangAlarmType) {
				return KindButtonPress
			}
		}
	}
	return KindMotion
}

// tagMatches compares a JSON-decoded value (which may be a float64,
// json.Number, or string) against a target string form.
func tagMatches(v interface{}, target string) bool {
	switch t := v.(type) {
	case string:
		return t == target
	case float64:
		// JSON numbers decode as float64; compare integer form.
		return formatInt(int64(t)) == target
	case int:
		return formatInt(int64(t)) == target
	case int64:
		return formatInt(t) == target
	}
	return false
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// firstImageThumbnail returns the first image url in the event's
// file_list, if any. Wyze file entries use type==1 for images.
func firstImageThumbnail(raw map[string]interface{}) string {
	files, ok := raw["file_list"].([]interface{})
	if !ok {
		return ""
	}
	for _, f := range files {
		fm, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		if tagMatches(fm["type"], "1") {
			if url, ok := fm["url"].(string); ok {
				return url
			}
		}
	}
	return ""
}

// deviceMAC extracts the camera identifier from a raw event. v2
// cameras use device_mac; v4 uses device_id.
func deviceMAC(raw map[string]interface{}) string {
	if id, ok := raw["device_id"].(string); ok && id != "" {
		return id
	}
	if mac, ok := raw["device_mac"].(string); ok {
		return mac
	}
	return ""
}

// eventTS returns the event timestamp (Wyze reports epoch ms).
func eventTS(raw map[string]interface{}) time.Time {
	switch v := raw["event_ts"].(type) {
	case float64:
		return time.UnixMilli(int64(v))
	case int64:
		return time.UnixMilli(v)
	}
	return time.Time{}
}

// eventID returns the dedupe key for an event.
func eventID(raw map[string]interface{}) string {
	if id, ok := raw["event_id"].(string); ok {
		return id
	}
	return ""
}

// Sink receives classified events. Any field may be nil.
type Sink struct {
	// OnButtonPress fires for a doorbell ring.
	OnButtonPress func(ev Event, camName string)
	// OnMotion fires for a motion/AI event.
	OnMotion func(ev Event, camName string)
}

// CameraLookup resolves a MAC to the bridge's camera name + info so
// sinks can publish under the normalized name. Returns ok=false when
// the MAC isn't a tracked camera.
type CameraLookup func(mac string) (name string, info wyzeapi.CameraInfo, ok bool)

// Poller periodically calls the Wyze event API, dedupes, classifies,
// and dispatches recent events to the configured sink.
type Poller struct {
	api      *wyzeapi.Client
	lookup   CameraLookup
	sink     Sink
	interval time.Duration
	// recentWindow bounds how old an event may be and still be acted
	// on, so a cold start doesn't replay a backlog of stale events.
	recentWindow time.Duration
	log          zerolog.Logger

	seen    map[string]struct{}
	seenBuf []string // insertion order for bounded eviction
	maxSeen int
	lastTS  int64 // epoch ms of newest processed event; API begin_time
}

// NewPoller constructs an event poller. interval<=0 disables it (the
// caller should not start it). recentWindow defaults to 30s.
func NewPoller(api *wyzeapi.Client, lookup CameraLookup, sink Sink, interval, recentWindow time.Duration, log zerolog.Logger) *Poller {
	if recentWindow <= 0 {
		recentWindow = 120 * time.Second
	}
	return &Poller{
		api:          api,
		lookup:       lookup,
		sink:         sink,
		interval:     interval,
		recentWindow: recentWindow,
		log:          log,
		seen:         make(map[string]struct{}),
		maxSeen:      256,
	}
}

// enabledDoorbellMACs and enabledMACs are provided by the caller via
// the MACs func passed to Run; kept flexible so the poller doesn't
// depend on the camera manager directly.

// Run polls until ctx is cancelled. macs returns the set of camera
// MACs to query on each tick (typically all enabled cameras).
func (p *Poller) Run(ctx context.Context, macs func() []string) {
	if p.interval <= 0 {
		return
	}
	p.log.Info().Dur("interval", p.interval).Msg("cloud event poller enabled")
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(macs())
		}
	}
}

// poll performs one event-list fetch + dispatch cycle. Exported logic
// lives in processEvents so tests can drive it without the API.
func (p *Poller) poll(macs []string) {
	if len(macs) == 0 {
		return
	}
	begin := p.lastTS
	if begin == 0 {
		// First poll: only look back one recent window to avoid a
		// stale backlog replay.
		begin = time.Now().Add(-p.recentWindow).UnixMilli()
	}
	end := time.Now().UnixMilli()

	raw, err := p.api.GetEventList(macs, begin, end)
	if err != nil {
		// Surface at warn: a persistent failure here means NO events
		// (motion or doorbell) will ever be delivered, so it must be
		// visible at the default log level, not hidden at debug.
		p.log.Warn().Err(err).Msg("get_event_list failed; no events will be delivered until this recovers")
		return
	}
	if len(raw) > 0 {
		p.log.Debug().Int("count", len(raw)).Msg("get_event_list returned events")
	}
	p.processEvents(raw, time.Now())
}

// processEvents dedupes, classifies, and dispatches a batch of raw
// events. now is injected for deterministic testing of the recent
// window filter.
func (p *Poller) processEvents(raw []map[string]interface{}, now time.Time) {
	for _, r := range raw {
		id := eventID(r)
		if id == "" {
			continue
		}
		if _, dup := p.seen[id]; dup {
			continue
		}
		p.markSeen(id)

		ts := eventTS(r)
		if !ts.IsZero() {
			if ms := ts.UnixMilli(); ms > p.lastTS {
				p.lastTS = ms
			}
		}

		kind := classify(r)

		// Diagnostic: log every event we receive with its raw
		// alarm-type so operators can confirm the classifier matches
		// their firmware. Visible at debug level.
		p.log.Debug().
			Str("event_id", id).
			Interface("event_value", r["event_value"]).
			Interface("event_tag_list", r["event_tag_list"]).
			Str("kind", kind.String()).
			Float64("age_s", now.Sub(ts).Seconds()).
			Msg("cloud event received")

		// Skip stale events (older than the recent window) so we don't
		// fire chimes/automations for a backlog on startup.
		if ts.IsZero() || now.Sub(ts) > p.recentWindow {
			p.log.Debug().
				Str("event_id", id).
				Float64("age_s", now.Sub(ts).Seconds()).
				Dur("window", p.recentWindow).
				Msg("event skipped: older than recent window")
			continue
		}

		mac := deviceMAC(r)
		if mac == "" {
			continue
		}
		name, _, ok := p.lookup(mac)
		if !ok {
			p.log.Debug().Str("mac", mac).Msg("event skipped: MAC not a tracked camera")
			continue
		}

		ev := Event{
			ID:        id,
			MAC:       mac,
			Kind:      kind,
			TS:        ts,
			Thumbnail: firstImageThumbnail(r),
		}

		switch ev.Kind {
		case KindButtonPress:
			if p.sink.OnButtonPress != nil {
				p.sink.OnButtonPress(ev, name)
			}
		default:
			if p.sink.OnMotion != nil {
				p.sink.OnMotion(ev, name)
			}
		}
	}
}

// markSeen records an event id for dedupe with bounded memory.
func (p *Poller) markSeen(id string) {
	p.seen[id] = struct{}{}
	p.seenBuf = append(p.seenBuf, id)
	if len(p.seenBuf) > p.maxSeen {
		evict := p.seenBuf[0]
		p.seenBuf = p.seenBuf[1:]
		delete(p.seen, evict)
	}
}
