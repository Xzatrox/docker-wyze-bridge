package mqtt

import (
	"encoding/json"
	"fmt"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
)

// PublishCameraState publishes the current state for a camera.
func (c *Client) PublishCameraState(cam *camera.Camera) {
	name := cam.Name()
	state := cam.GetState()

	stateStr := "disconnected"
	powerStr := "off"
	if state == camera.StateStreaming {
		stateStr = "connected"
		powerStr = "on"
	}

	c.publish(fmt.Sprintf("%s/%s/state", c.topic, name), stateStr, true)
	c.publish(fmt.Sprintf("%s/%s/power", c.topic, name), powerStr, true)
	c.publish(fmt.Sprintf("%s/%s/net_mode", c.topic, name), "lan", true)
	c.publish(fmt.Sprintf("%s/%s/quality", c.topic, name), cam.GetQuality(), true)

	audioStr := "false"
	if cam.GetAudioOn() {
		audioStr = "true"
	}
	c.publish(fmt.Sprintf("%s/%s/audio", c.topic, name), audioStr, true)
}

// PublishCameraInfo publishes static camera information.
func (c *Client) PublishCameraInfo(cam *camera.Camera) {
	name := cam.Name()
	info := cam.GetInfo()

	cameraInfo := map[string]string{
		"ip":         info.LanIP,
		"model":      info.Model,
		"fw_version": info.FWVersion,
		"mac":        info.MAC,
	}
	data, _ := json.Marshal(cameraInfo)
	c.publish(fmt.Sprintf("%s/%s/camera_info", c.topic, name), string(data), true)

	streamInfo := map[string]string{
		"rtsp_url":   fmt.Sprintf("rtsp://%s:8554/%s", c.bridgeIP, name),
		"webrtc_url": fmt.Sprintf("http://%s:8889/%s", c.bridgeIP, name),
		"hls_url":    fmt.Sprintf("http://%s:1984/api/stream.m3u8?src=%s", c.bridgeIP, name),
	}
	data, _ = json.Marshal(streamInfo)
	c.publish(fmt.Sprintf("%s/%s/stream_info", c.topic, name), string(data), true)
}

// PublishThumbnail publishes a JPEG snapshot to the camera's thumbnail topic.
func (c *Client) PublishThumbnail(camName string, jpeg []byte) {
	c.publishBytes(fmt.Sprintf("%s/%s/thumbnail", c.topic, camName), jpeg, true)
}

// PublishButtonPress publishes a doorbell button-press to the camera's
// HA `event` entity state topic. The payload is JSON {"event_type":
// "pressed"}; HA fires an event trigger on each message. Not retained
// — a ring is a momentary event, not a state to restore on restart.
func (c *Client) PublishButtonPress(camName string) {
	c.publish(fmt.Sprintf("%s/%s/event/button", c.topic, camName), `{"event_type":"pressed"}`, false)
}

// PublishMotion publishes a motion event to the camera's motion topic
// as a momentary "1" (not retained). Kept lightweight; the primary
// motion signal remains the camera's own detection settings.
func (c *Client) PublishMotion(camName string) {
	c.publish(fmt.Sprintf("%s/%s/motion", c.topic, camName), "1", false)
}

// PublishBridgeState publishes the bridge online/offline state.
func (c *Client) PublishBridgeState(online bool) {
	state := "offline"
	if online {
		state = "online"
	}
	c.publish(fmt.Sprintf("%s/bridge/state", c.topic), state, true)
}
