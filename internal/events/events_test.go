package events

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]interface{}
		want Kind
	}{
		{
			name: "button press via event_value string (10=DOORBELL_RANG)",
			raw:  map[string]interface{}{"event_value": "10"},
			want: KindButtonPress,
		},
		{
			name: "button press via event_value number",
			raw:  map[string]interface{}{"event_value": float64(10)},
			want: KindButtonPress,
		},
		{
			name: "button press echoed in tag_list",
			raw:  map[string]interface{}{"event_tag_list": []interface{}{"10"}},
			want: KindButtonPress,
		},
		{
			name: "motion via event_value 13",
			raw:  map[string]interface{}{"event_value": float64(13)},
			want: KindMotion,
		},
		{
			name: "motion via event_value 1",
			raw:  map[string]interface{}{"event_value": "1"},
			want: KindMotion,
		},
		{
			name: "face (12) is not a button press",
			raw:  map[string]interface{}{"event_value": "12"},
			want: KindMotion,
		},
		{
			name: "motion with no fields",
			raw:  map[string]interface{}{},
			want: KindMotion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.raw); got != tt.want {
				t.Errorf("classify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	if KindButtonPress.String() != "button_press" {
		t.Errorf("KindButtonPress = %q", KindButtonPress.String())
	}
	if KindMotion.String() != "motion" {
		t.Errorf("KindMotion = %q", KindMotion.String())
	}
}

func TestDeviceMAC(t *testing.T) {
	// device_id (v4) takes precedence.
	if got := deviceMAC(map[string]interface{}{"device_id": "AABB", "device_mac": "CCDD"}); got != "AABB" {
		t.Errorf("deviceMAC precedence = %q, want AABB", got)
	}
	// falls back to device_mac (v2).
	if got := deviceMAC(map[string]interface{}{"device_mac": "CCDD"}); got != "CCDD" {
		t.Errorf("deviceMAC fallback = %q, want CCDD", got)
	}
	if got := deviceMAC(map[string]interface{}{}); got != "" {
		t.Errorf("deviceMAC empty = %q, want empty", got)
	}
}

func TestFirstImageThumbnail(t *testing.T) {
	raw := map[string]interface{}{
		"file_list": []interface{}{
			map[string]interface{}{"type": float64(2), "url": "video.mp4"},
			map[string]interface{}{"type": float64(1), "url": "image.jpg"},
		},
	}
	if got := firstImageThumbnail(raw); got != "image.jpg" {
		t.Errorf("firstImageThumbnail = %q, want image.jpg", got)
	}
	if got := firstImageThumbnail(map[string]interface{}{}); got != "" {
		t.Errorf("firstImageThumbnail empty = %q", got)
	}
}

func TestFormatInt(t *testing.T) {
	cases := map[int64]string{0: "0", 13: "13", -12: "-12", 100: "100"}
	for in, want := range cases {
		if got := formatInt(in); got != want {
			t.Errorf("formatInt(%d) = %q, want %q", in, got, want)
		}
	}
}

// fakeLookup resolves any MAC to a fixed camera name.
func fakeLookup(name string) CameraLookup {
	return func(mac string) (string, wyzeapi.CameraInfo, bool) {
		if mac == "" {
			return "", wyzeapi.CameraInfo{}, false
		}
		return name, wyzeapi.CameraInfo{MAC: mac, Model: "HL_DB2"}, true
	}
}

func newTestPoller(sink Sink) *Poller {
	return NewPoller(nil, fakeLookup("front_door"), sink, time.Second, 30*time.Second, zerolog.Nop())
}

func TestProcessEvents_ButtonPressDispatch(t *testing.T) {
	var pressed, motion int
	p := newTestPoller(Sink{
		OnButtonPress: func(ev Event, name string) {
			pressed++
			if name != "front_door" {
				t.Errorf("button camName = %q", name)
			}
			if ev.Kind != KindButtonPress {
				t.Errorf("ev.Kind = %v", ev.Kind)
			}
		},
		OnMotion: func(ev Event, name string) { motion++ },
	})

	now := time.Now()
	raw := []map[string]interface{}{
		{
			"event_id":       "e1",
			"device_mac":     "AABBCC",
			"event_ts":       float64(now.Add(-2 * time.Second).UnixMilli()),
			"event_value":     "10",
		},
	}
	p.processEvents(raw, now)

	if pressed != 1 {
		t.Errorf("button press dispatched %d times, want 1", pressed)
	}
	if motion != 0 {
		t.Errorf("motion dispatched %d times, want 0", motion)
	}
}

func TestProcessEvents_MotionDispatch(t *testing.T) {
	var pressed, motion int
	p := newTestPoller(Sink{
		OnButtonPress: func(ev Event, name string) { pressed++ },
		OnMotion:      func(ev Event, name string) { motion++ },
	})
	now := time.Now()
	raw := []map[string]interface{}{
		{
			"event_id":   "m1",
			"device_mac": "AABBCC",
			"event_ts":   float64(now.Add(-1 * time.Second).UnixMilli()),
		},
	}
	p.processEvents(raw, now)
	if motion != 1 || pressed != 0 {
		t.Errorf("motion=%d pressed=%d, want motion=1 pressed=0", motion, pressed)
	}
}

func TestProcessEvents_Dedupe(t *testing.T) {
	var count int
	p := newTestPoller(Sink{
		OnButtonPress: func(ev Event, name string) { count++ },
	})
	now := time.Now()
	ev := map[string]interface{}{
		"event_id":       "dup",
		"device_mac":     "AABBCC",
		"event_ts":       float64(now.Add(-1 * time.Second).UnixMilli()),
		"event_value":     "10",
	}
	// Same event id delivered twice across two polls.
	p.processEvents([]map[string]interface{}{ev}, now)
	p.processEvents([]map[string]interface{}{ev}, now)
	if count != 1 {
		t.Errorf("dispatched %d times, want 1 (deduped)", count)
	}
}

func TestProcessEvents_StaleWindowSkipped(t *testing.T) {
	var count int
	p := newTestPoller(Sink{
		OnButtonPress: func(ev Event, name string) { count++ },
	})
	now := time.Now()
	// Event 90s old — outside the 30s recent window.
	raw := []map[string]interface{}{
		{
			"event_id":       "old",
			"device_mac":     "AABBCC",
			"event_ts":       float64(now.Add(-90 * time.Second).UnixMilli()),
			"event_value":     "10",
		},
	}
	p.processEvents(raw, now)
	if count != 0 {
		t.Errorf("stale event dispatched %d times, want 0", count)
	}
	// But lastTS should still advance so we don't re-query it.
	if p.lastTS == 0 {
		t.Error("lastTS should advance even for stale events")
	}
}

func TestProcessEvents_UnknownCameraSkipped(t *testing.T) {
	var count int
	p := NewPoller(nil, func(mac string) (string, wyzeapi.CameraInfo, bool) {
		return "", wyzeapi.CameraInfo{}, false // never resolves
	}, Sink{OnButtonPress: func(ev Event, name string) { count++ }}, time.Second, 30*time.Second, zerolog.Nop())

	now := time.Now()
	raw := []map[string]interface{}{
		{
			"event_id":       "e1",
			"device_mac":     "UNKNOWN",
			"event_ts":       float64(now.UnixMilli()),
			"event_value":     "10",
		},
	}
	p.processEvents(raw, now)
	if count != 0 {
		t.Errorf("unknown camera dispatched %d times, want 0", count)
	}
}

func TestMarkSeen_BoundedEviction(t *testing.T) {
	p := newTestPoller(Sink{})
	p.maxSeen = 3
	for _, id := range []string{"a", "b", "c", "d"} {
		p.markSeen(id)
	}
	if len(p.seen) != 3 {
		t.Errorf("seen size = %d, want 3 (bounded)", len(p.seen))
	}
	if _, ok := p.seen["a"]; ok {
		t.Error("oldest id 'a' should have been evicted")
	}
	if _, ok := p.seen["d"]; !ok {
		t.Error("newest id 'd' should be retained")
	}
}
