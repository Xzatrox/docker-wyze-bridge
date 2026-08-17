package wyzeapi

import (
	"fmt"
)

// RunAction sends a run_action command to a camera via the Wyze cloud API.
func (c *Client) RunAction(cam CameraInfo, action string) error {
	if err := c.EnsureAuth(); err != nil {
		return err
	}

	payload := c.authenticatedPayload("run_action")
	payload["action_params"] = map[string]interface{}{}
	payload["action_key"] = action
	payload["instance_id"] = cam.MAC
	payload["provider_key"] = cam.Model
	payload["custom_string"] = ""

	_, err := c.postJSON(c.WyzeURL+"/v2/auto/run_action", c.defaultHeaders(), payload)
	if err != nil {
		return fmt.Errorf("run_action: %w", err)
	}
	return nil
}

// GetEventList fetches recent events for the given MAC addresses.
// Uses the v2 endpoint (same as wyzeapy) with newest-first ordering and
// a fixed 1-hour lookback, which correctly returns each button press as
// a separate event even while the recording is still open (end_time=0).
func (c *Client) GetEventList(macs []string, beginTimeMS, endTimeMS int64) ([]map[string]interface{}, error) {
	if err := c.EnsureAuth(); err != nil {
		return nil, err
	}

	// Deduplicate MACs
	macSet := make(map[string]bool)
	for _, m := range macs {
		macSet[m] = true
	}
	uniqueMACs := make([]string, 0, len(macSet))
	for m := range macSet {
		uniqueMACs = append(uniqueMACs, m)
	}

	// Use the v2 endpoint with wyzeapy-style parameters:
	//   - order_by: 2 (newest first) — returns each press as its own event
	//   - event_value_list: [] (empty = no filter) — returns ALL event types
	//     so button_press (10) isn't accidentally filtered out by firmware quirks
	//   - begin_time: caller-supplied (typically now-recentWindow)
	payload := c.authenticatedPayload("get_event_list")
	payload["count"] = 20
	payload["order_by"] = 2
	payload["begin_time"] = beginTimeMS
	payload["end_time"] = endTimeMS
	payload["device_mac_list"] = uniqueMACs
	payload["event_value_list"] = []interface{}{}
	payload["event_tag_list"] = []interface{}{}
	payload["event_type"] = ""
	payload["device_mac"] = ""

	resp, err := c.postJSON(c.WyzeURL+"/v2/device/get_event_list", c.defaultHeaders(), payload)
	if err != nil {
		return nil, fmt.Errorf("get_event_list: %w", err)
	}

	// v2 response shape: {"code":"1","data":{"event_list":[...]}}
	data, ok := resp["event_list"].([]interface{})
	if !ok {
		// also try nested under "data"
		if d, ok2 := resp["data"].(map[string]interface{}); ok2 {
			data, _ = d["event_list"].([]interface{})
		}
	}

	var result []map[string]interface{}
	for _, e := range data {
		if m, ok := e.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result, nil
}
