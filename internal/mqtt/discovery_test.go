package mqtt

import (
	"encoding/json"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

func TestHADevice(t *testing.T) {
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Nickname:  "Front Door",
		Model:     "HL_CAM4",
		MAC:       "AABBCCDDEEFF",
		FWVersion: "4.52.9.4188",
	}, "hd", true, false)

	dev := haDevice(cam)

	ids, ok := dev["identifiers"].([]string)
	if !ok || len(ids) != 1 || ids[0] != "wyze_AABBCCDDEEFF" {
		t.Errorf("identifiers = %v", dev["identifiers"])
	}
	if dev["name"] != "Front Door" {
		t.Errorf("name = %v", dev["name"])
	}
	if dev["model"] != "HL_CAM4" {
		t.Errorf("model = %v", dev["model"])
	}
	if dev["manufacturer"] != "Wyze" {
		t.Errorf("manufacturer = %v", dev["manufacturer"])
	}
	if dev["sw_version"] != "4.52.9.4188" {
		t.Errorf("sw_version = %v", dev["sw_version"])
	}
}

func TestDiscoveryConfigJSON(t *testing.T) {
	// Test that discovery config generates valid JSON
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Name:      "front_door",
		Nickname:  "Front Door",
		Model:     "HL_CAM4",
		MAC:       "AABBCCDDEEFF",
		FWVersion: "4.52.9.4188",
	}, "hd", true, false)

	device := haDevice(cam)

	config := map[string]interface{}{
		"name":                  cam.Info.Nickname,
		"unique_id":             "wyze_" + cam.Info.MAC,
		"topic":                 "wyzebridge/front_door/",
		"availability_topic":    "wyzebridge/front_door/state",
		"payload_available":     "connected",
		"payload_not_available": "disconnected",
		"device":                device,
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Verify it round-trips
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["unique_id"] != "wyze_AABBCCDDEEFF" {
		t.Errorf("unique_id = %v", parsed["unique_id"])
	}
	if parsed["payload_available"] != "connected" {
		t.Errorf("payload_available = %v", parsed["payload_available"])
	}
}

func TestDoorbellButtonGating(t *testing.T) {
	// The button event entity + device trigger are only for doorbells.
	doorbell := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "front_doorbell", Nickname: "Front Doorbell", Model: "HL_DB2", MAC: "AABBCCDDEEFF",
	}, "hd", true, false)
	if !doorbell.Info.IsDoorbell() {
		t.Fatal("HL_DB2 should report IsDoorbell()=true")
	}

	notDoorbell := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "backyard", Nickname: "Backyard", Model: "WYZE_CAKP2JFUS", MAC: "112233445566",
	}, "hd", true, false)
	if notDoorbell.Info.IsDoorbell() {
		t.Error("WYZE_CAKP2JFUS should report IsDoorbell()=false")
	}
}

func TestDeviceTriggerDiscoveryShape(t *testing.T) {
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "front_doorbell", Nickname: "Front Doorbell", Model: "HL_DB2", MAC: "AABBCCDDEEFF",
	}, "hd", true, false)
	device := haDevice(cam)

	// Mirrors the device_automation config emitted in PublishDiscovery.
	config := map[string]interface{}{
		"automation_type": "trigger",
		"type":            "button_short_press",
		"subtype":         "button",
		"topic":           "wyzebridge/front_doorbell/event/button",
		"payload":         "pressed",
		"value_template":  "{{ value_json.event_type }}",
		"device":          device,
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["automation_type"] != "trigger" {
		t.Errorf("automation_type = %v, want trigger", parsed["automation_type"])
	}
	if parsed["payload"] != "pressed" {
		t.Errorf("payload = %v, want pressed", parsed["payload"])
	}
	if parsed["topic"] != "wyzebridge/front_doorbell/event/button" {
		t.Errorf("topic = %v", parsed["topic"])
	}
}

func TestEventEntityDiscoveryShape(t *testing.T) {
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Name: "front_doorbell", Nickname: "Front Doorbell", Model: "HL_DB2", MAC: "AABBCCDDEEFF",
	}, "hd", true, false)
	device := haDevice(cam)

	config := map[string]interface{}{
		"name":         cam.Info.Nickname + " Button",
		"unique_id":    "wyze_" + cam.Info.MAC + "_button",
		"state_topic":  "wyzebridge/front_doorbell/event/button",
		"event_types":  []string{"pressed"},
		"device_class": "doorbell",
		"device":       device,
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	types, ok := parsed["event_types"].([]interface{})
	if !ok || len(types) != 1 || types[0] != "pressed" {
		t.Errorf("event_types = %v", parsed["event_types"])
	}
	if parsed["device_class"] != "doorbell" {
		t.Errorf("device_class = %v", parsed["device_class"])
	}
}

func TestQualitySelectDiscovery(t *testing.T) {
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Name:     "backyard",
		Nickname: "Backyard",
		Model:    "WYZE_CAKP2JFUS",
		MAC:      "112233445566",
	}, "hd", true, false)

	device := haDevice(cam)

	config := map[string]interface{}{
		"name":          cam.Info.Nickname + " Quality",
		"unique_id":     "wyze_112233445566_quality",
		"state_topic":   "wyzebridge/backyard/quality",
		"command_topic": "wyzebridge/backyard/set/quality",
		"options":       []string{"hd", "sd"},
		"device":        device,
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)

	opts, ok := parsed["options"].([]interface{})
	if !ok || len(opts) != 2 {
		t.Errorf("options = %v", parsed["options"])
	}
}

func TestAudioSwitchDiscovery(t *testing.T) {
	cam := camera.NewCamera(wyzeapi.CameraInfo{
		Name:     "garage",
		Nickname: "Garage",
		Model:    "HL_PAN3",
		MAC:      "AABB99887766",
	}, "hd", true, false)

	device := haDevice(cam)

	config := map[string]interface{}{
		"name":          "Garage Audio",
		"unique_id":     "wyze_AABB99887766_audio",
		"state_topic":   "wyzebridge/garage/audio",
		"command_topic": "wyzebridge/garage/set/audio",
		"payload_on":    "true",
		"payload_off":   "false",
		"state_on":      "true",
		"state_off":     "false",
		"device":        device,
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)

	if parsed["payload_on"] != "true" {
		t.Errorf("payload_on = %v", parsed["payload_on"])
	}
	if parsed["state_off"] != "false" {
		t.Errorf("state_off = %v", parsed["state_off"])
	}
}
