package go2rtcmgr

import (
	"os"
	"strings"
	"testing"
)

// go2rtc customization surface — issues #123 (API auth), #108 (port
// collision with Frigate on 1984), and #106 (pass-through extra streams).

func TestConfigBuilder_DefaultPorts(t *testing.T) {
	b := NewConfigBuilder("info", "", "")
	cfg := b.Build()
	if cfg.API.Listen != ":1984" {
		t.Errorf("API.Listen = %q, want :1984", cfg.API.Listen)
	}
	if cfg.RTSP.Listen != ":8554" {
		t.Errorf("RTSP.Listen = %q, want :8554", cfg.RTSP.Listen)
	}
	if cfg.WebRTC.Listen != ":8889" {
		t.Errorf("WebRTC.Listen = %q, want :8889", cfg.WebRTC.Listen)
	}
	if b.APIPort() != 1984 {
		t.Errorf("APIPort() = %d, want 1984", b.APIPort())
	}
}

func TestConfigBuilder_OverridePorts(t *testing.T) {
	b := NewConfigBuilder("info", "", "")
	b.SetAPIPort(1985)
	b.SetRTSPPort(8555)
	b.SetWebRTCPort(8890)
	cfg := b.Build()
	if cfg.API.Listen != ":1985" || cfg.RTSP.Listen != ":8555" || cfg.WebRTC.Listen != ":8890" {
		t.Errorf("listen ports = api=%s rtsp=%s webrtc=%s", cfg.API.Listen, cfg.RTSP.Listen, cfg.WebRTC.Listen)
	}
	if b.APIPort() != 1985 {
		t.Errorf("APIPort() = %d, want 1985", b.APIPort())
	}
}

func TestConfigBuilder_ZeroPortKeepsDefault(t *testing.T) {
	// So callers can pass cfg.Go2RTCAPIPort unconditionally; a zero
	// from the env just falls through to the builtin default.
	b := NewConfigBuilder("info", "", "")
	b.SetAPIPort(0)
	b.SetRTSPPort(0)
	b.SetWebRTCPort(0)
	if b.APIPort() != 1984 {
		t.Errorf("APIPort() = %d after zero override, want 1984", b.APIPort())
	}
}

func TestConfigBuilder_WebRTCCandidateUsesConfiguredPort(t *testing.T) {
	b := NewConfigBuilder("info", "", "192.168.1.50")
	b.SetWebRTCPort(8891)
	cfg := b.Build()
	if len(cfg.WebRTC.Candidates) != 1 || cfg.WebRTC.Candidates[0] != "192.168.1.50:8891" {
		t.Errorf("candidates = %v, want [192.168.1.50:8891]", cfg.WebRTC.Candidates)
	}
}

func TestConfigBuilder_APIAuth(t *testing.T) {
	b := NewConfigBuilder("info", "", "")
	b.SetAPIAuth("admin", "hunter2")
	cfg := b.Build()
	if cfg.API.Username != "admin" || cfg.API.Password != "hunter2" {
		t.Errorf("API auth = %q/%q, want admin/hunter2", cfg.API.Username, cfg.API.Password)
	}
	if u, p := b.APIUsername(), b.APIPassword(); u != "admin" || p != "hunter2" {
		t.Errorf("accessor = %q/%q, want admin/hunter2", u, p)
	}
}

func TestConfigBuilder_APIAuth_EmptyIsNoOp(t *testing.T) {
	// Partial creds shouldn't half-configure auth; the operator can
	// leave one env var unset without lock-out.
	b := NewConfigBuilder("info", "", "")
	b.SetAPIAuth("admin", "")
	b.SetAPIAuth("", "hunter2")
	cfg := b.Build()
	if cfg.API.Username != "" || cfg.API.Password != "" {
		t.Errorf("half-set auth leaked: %q/%q", cfg.API.Username, cfg.API.Password)
	}
}

func TestConfigBuilder_ExtraStreams_MergedWithCameras(t *testing.T) {
	b := NewConfigBuilder("info", "", "")
	b.AddExtraStream(StreamEntry{Name: "frigate_front", URL: "rtsp://frigate:8554/front"})
	b.AddExtraStream(StreamEntry{Name: "onvif", URL: "onvif://user:pass@192.168.1.30"})
	b.AddStream(StreamEntry{Name: "wyze_v3", URL: "wyze://10.0.0.5?uid=X&enr=Y"})
	cfg := b.Build()
	if len(cfg.Streams) != 3 {
		t.Fatalf("streams = %d, want 3 (2 extra + 1 camera)", len(cfg.Streams))
	}
	for _, name := range []string{"frigate_front", "onvif", "wyze_v3"} {
		if _, ok := cfg.Streams[name]; !ok {
			t.Errorf("missing stream %q", name)
		}
	}
}

func TestConfigBuilder_ExtraStream_YieldsToCameraNameCollision(t *testing.T) {
	// Cameras are the primary product; if an operator misconfigures
	// an extra stream to overlap with a camera name, the camera wins
	// because extras are rendered first and cameras iterate second.
	b := NewConfigBuilder("info", "", "")
	b.AddExtraStream(StreamEntry{Name: "front_door", URL: "rtsp://elsewhere/front"})
	b.AddStream(StreamEntry{Name: "front_door", URL: "wyze://10.0.0.5"})
	cfg := b.Build()
	got, ok := cfg.Streams["front_door"].([]string)
	if !ok {
		t.Fatalf("front_door value type = %T, want []string", cfg.Streams["front_door"])
	}
	if len(got) != 1 || got[0] != "wyze://10.0.0.5" {
		t.Errorf("collision winner = %v, want [wyze://10.0.0.5]", got)
	}
}

func TestParseExtraStreams(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []StreamEntry
	}{
		{"empty", "", nil},
		{"single comma-form", "frigate=rtsp://frigate:8554/front",
			[]StreamEntry{{Name: "frigate", URL: "rtsp://frigate:8554/front"}}},
		{"multi comma-form", "a=rtsp://a,b=rtsp://b",
			[]StreamEntry{{Name: "a", URL: "rtsp://a"}, {Name: "b", URL: "rtsp://b"}}},
		{"multi semicolon-form", "a=rtsp://a;b=rtsp://b",
			[]StreamEntry{{Name: "a", URL: "rtsp://a"}, {Name: "b", URL: "rtsp://b"}}},
		{"whitespace around name+url ok",
			"  a  =  rtsp://a  ,  b=onvif://b  ",
			[]StreamEntry{{Name: "a", URL: "rtsp://a"}, {Name: "b", URL: "onvif://b"}}},
		{"missing = is skipped, others survive",
			"garbled,ok=rtsp://ok",
			[]StreamEntry{{Name: "ok", URL: "rtsp://ok"}}},
		{"trailing = is skipped",
			"ok=rtsp://ok,dangling=",
			[]StreamEntry{{Name: "ok", URL: "rtsp://ok"}}},
		{"URL containing = survives (only first = splits)",
			"probe=onvif://u:p@host?opt=1",
			[]StreamEntry{{Name: "probe", URL: "onvif://u:p@host?opt=1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseExtraStreams(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestConfigBuilder_AppendRawYAML(t *testing.T) {
	b := NewConfigBuilder("info", "", "")
	b.AppendRawYAML("publish:\n  wyze_v3: rtmp://youtube/live/KEY\n")
	tmp, err := os.CreateTemp("", "go2rtc-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()
	if err := b.WriteConfig(tmp.Name()); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	body, _ := os.ReadFile(tmp.Name())
	s := string(body)
	if !strings.Contains(s, "publish:") || !strings.Contains(s, "wyze_v3: rtmp://youtube") {
		t.Errorf("raw YAML fragment missing from output:\n%s", s)
	}
	if !strings.Contains(s, "managed by wyze-bridge") {
		t.Error("expected marker separating managed from operator YAML")
	}
}

func TestConfigBuilder_AppendRawYAML_EmptyStaysClean(t *testing.T) {
	// Empty or whitespace-only extra YAML must NOT emit the marker or
	// a stray newline — the operator shouldn't see debris when the
	// env var is unset.
	b := NewConfigBuilder("info", "", "")
	b.AppendRawYAML("   \n\t\n  ")
	tmp, err := os.CreateTemp("", "go2rtc-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()
	if err := b.WriteConfig(tmp.Name()); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	body, _ := os.ReadFile(tmp.Name())
	if strings.Contains(string(body), "managed by wyze-bridge") {
		t.Errorf("marker emitted for whitespace-only extra YAML:\n%s", body)
	}
}
