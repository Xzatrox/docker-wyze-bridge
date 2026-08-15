package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// TestHandleDashboardYAML_Smoke renders the auto-generated Lovelace
// YAML and checks the structural skeleton: title, summary glance,
// markdown card. With no cameras registered there are still bridge
// sensors to render.
func TestHandleDashboardYAML_Smoke(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest("GET", "/dashboard.yaml", nil)
	w := httptest.NewRecorder()
	srv.handleDashboardYAML(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	mustHave := []string{
		"title: Wyze Bridge",
		"type: glance",
		"sensor.bridge_cameras",
		"sensor.bridge_streaming",
		"sensor.bridge_recordings_size",
		"type: markdown",
	}
	for _, want := range mustHave {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard yaml\n--- body ---\n%s", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("content-type = %q, want application/yaml", ct)
	}
}

// TestHandleDashboardYAML_DoorbellCard verifies that a doorbell
// camera gets the button `event` entity on its picture-glance plus a
// dedicated doorbell entities card, while a non-doorbell camera does
// not.
func TestHandleDashboardYAML_DoorbellCard(t *testing.T) {
	srv, _ := testServer(t)

	doorbell := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "front_doorbell", Nickname: "Front Doorbell", Model: "HL_DB2", MAC: "AABBCCDDEEFF",
	}, "hd", true, false)
	srv.camMgr.InjectCamera("front_doorbell", doorbell)

	plain := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "backyard", Nickname: "Backyard", Model: "WYZE_CAKP2JFUS", MAC: "112233445566",
	}, "hd", true, false)
	srv.camMgr.InjectCamera("backyard", plain)

	req := httptest.NewRequest("GET", "/dashboard.yaml", nil)
	w := httptest.NewRecorder()
	srv.handleDashboardYAML(w, req)

	body := w.Body.String()

	// Doorbell entity + dedicated card must be present.
	mustHave := []string{
		"event.wyze_aabbccddeeff_button",
		"Front Doorbell Doorbell",
		"name: Last Ring",
		"camera.wyze_aabbccddeeff",
	}
	for _, want := range mustHave {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard yaml\n--- body ---\n%s", want, body)
		}
	}

	// The non-doorbell camera must NOT get a button event entity.
	if strings.Contains(body, "event.wyze_112233445566_button") {
		t.Error("non-doorbell camera should not have a button event entity")
	}
}

// TestYAMLQuote covers the few characters that need explicit
// double-quoting in a YAML scalar (the rest pass through unchanged).
func TestYAMLQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"has space", "has space"},     // space alone isn't a special char
		{"colon: here", `"colon: here"`},
		{`with"quote`, `"with\"quote"`},
		{"", `""`},
	}
	for _, c := range cases {
		got := yamlQuote(c.in)
		if got != c.want {
			t.Errorf("yamlQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
