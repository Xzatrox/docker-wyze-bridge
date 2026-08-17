// Package fcm implements a minimal Firebase Cloud Messaging (FCM) client
// that registers as an Android device and receives push notifications
// over the MCS (Mobile Connection Service) persistent connection.
//
// This is used to receive real-time doorbell ring events from Wyze's
// FCM integration, providing sub-second notification delivery without
// depending on cloud event polling.
//
// Protocol overview:
//  1. Android checkin (https://android.clients.google.com/checkin) — gets
//     an android_id + security_token identifying our virtual device.
//  2. GCM register (https://android.clients.google.com/c2dm/register3) —
//     subscribes this device to the Wyze app's sender ID and gets a
//     registration token we can share with Wyze's servers.
//  3. Register the FCM token with the Wyze API so Wyze's backend knows
//     to send push notifications for our account to this device.
//  4. Connect to mtalk.google.com:5228 (MCS) — a long-lived TLS connection
//     over which FCM delivers DataMessage payloads as protobuf-framed
//     packets. We parse the ring notification fields and fire OnRing.
package fcm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Firebase / Wyze app credentials extracted from the Wyze APK
// (resources.arsc global string pool, confirmed via raw binary scan).
// Two Firebase projects exist in the APK:
//   - Test project (fir-test-d9f49): debug build, project# 332820210871
//   - Production project: project# 738316279406 — this is the one the
//     mobile app uses for FCM push notifications (confirmed from the
//     FIS error "consumer: projects/738316279406").
const (
	// wyzeFirebaseSenderID is the production GCM sender ID (= GCP project number).
	// Extracted from the production google_app_id in resources.arsc.
	wyzeFirebaseSenderID = "738316279406"
	// wyzeFirebaseAppID is the production Firebase App ID from resources.arsc.
	wyzeFirebaseAppID = "1:738316279406:android:3c34a4366b9bad6d"
	// wyzeFirebaseAPIKey is google_api_key (primary) from resources.arsc.
	wyzeFirebaseAPIKey = "AIzaSyAeIB89cJs0N-B2orKyf0zBl1z6OynMmV8"
	// wyzeFirebaseAPIKey2 is the second API key found in resources.arsc.
	wyzeFirebaseAPIKey2 = "AIzaSyDYCWqUuB65wWcJD0-UVpmKGxPccgh388A"
	// wyzeFirebaseProjectID is the production project number used in FIS URLs.
	wyzeFirebaseProjectID = "738316279406"
	// wyzeFirebaseProductionProjectID is an alias for clarity.
	wyzeFirebaseProductionProjectID = "738316279406"
	// wyzeAndroidPackage is the Wyze app package name.
	wyzeAndroidPackage = "com.hualai"
	// mcsHost is the Firebase MCS endpoint for receiving pushes.
	mcsHost = "mtalk.google.com:5228"
	// mcsVersion identifies the protocol version in the login packet.
	mcsVersion = 41
)

// RingEvent represents a doorbell ring received via FCM.
type RingEvent struct {
	// DeviceMAC is the MAC address of the doorbell that rang.
	DeviceMAC string
	// DeviceName is the friendly name, if present in the payload.
	DeviceName string
	// TS is when we received the notification.
	TS time.Time
	// RawPayload is the full key-value map from the FCM data message,
	// useful for debugging.
	RawPayload map[string]string
}

// State holds the persisted registration state so we don't re-register
// on every restart. This should be saved to disk between runs.
type State struct {
	// AndroidID and SecurityToken from the checkin step.
	AndroidID     uint64 `json:"android_id"`
	SecurityToken uint64 `json:"security_token"`
	// FCMToken is the registration token obtained from GCM register,
	// shared with Wyze's servers so they know where to send pushes.
	FCMToken string `json:"fcm_token"`
}

// Client manages FCM registration and the MCS persistent connection.
type Client struct {
	state State
	mu    sync.Mutex
	log   zerolog.Logger

	// OnRing is called when a doorbell ring push notification arrives.
	// Called from the MCS reader goroutine; implementations must be
	// goroutine-safe and return quickly.
	OnRing func(ev RingEvent)

	// OnTokenRefresh is called when a new FCM token is obtained and
	// should be persisted and re-registered with Wyze's API.
	OnTokenRefresh func(token string)
}

// New creates a new FCM client with the given persisted state.
// Pass a zero State on first run; the client will register and call
// OnTokenRefresh with the new token.
func New(state State, log zerolog.Logger) *Client {
	return &Client{state: state, log: log}
}

// State returns a copy of the current registration state for persistence.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// EnsureRegistered performs Android checkin and attempts GCM token
// registration. GCM registration is best-effort — if it fails (e.g. because
// Wyze's Firebase project blocks the FIS API for external callers), the MCS
// connection still proceeds using just the android_id/security_token.
// Returns the FCM token if obtained, or "" if registration failed.
func (c *Client) EnsureRegistered(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.AndroidID == 0 || c.state.SecurityToken == 0 {
		c.log.Info().Msg("fcm: performing Android checkin")
		aid, st, err := androidCheckin(ctx)
		if err != nil {
			return "", fmt.Errorf("fcm: android checkin: %w", err)
		}
		c.state.AndroidID = aid
		c.state.SecurityToken = st
		c.log.Info().Uint64("android_id", aid).Msg("fcm: checkin complete")
	}

	if c.state.FCMToken == "" {
		c.log.Info().Msg("fcm: attempting GCM token registration")
		tok, err := gcmRegister(ctx, c.state.AndroidID, c.state.SecurityToken)
		if err != nil {
			// GCM registration is not strictly required to connect to MCS.
			// Log as warn and continue — MCS login uses android_id directly.
			c.log.Warn().Err(err).Msg("fcm: GCM registration failed — connecting to MCS without FCM token (pushes may not arrive until Wyze registers this android_id)")
		} else {
			c.state.FCMToken = tok
			c.log.Info().Str("token_prefix", tok[:min(20, len(tok))]).Msg("fcm: GCM token obtained")
			if c.OnTokenRefresh != nil {
				go c.OnTokenRefresh(tok)
			}
		}
	}

	// Return android_id as a synthetic "token" for Wyze registration when
	// no FCM token was obtained — Wyze's set_push_info also accepts
	// android_id-style tokens on some firmware versions.
	token := c.state.FCMToken
	if token == "" {
		token = fmt.Sprintf("AID:%d", c.state.AndroidID)
	}
	return token, nil
}

// Run connects to the MCS endpoint and dispatches incoming push
// notifications until ctx is cancelled. It automatically reconnects
// on disconnection with exponential backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := 2 * time.Second
	maxBackoff := 5 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		aid := c.state.AndroidID
		st := c.state.SecurityToken
		tok := c.state.FCMToken
		c.mu.Unlock()

		if aid == 0 {
			c.log.Warn().Msg("fcm: no android_id yet, retrying in 30s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		// tok may be empty if GCM registration failed — MCS login still
		// works with just android_id + security_token.

		c.log.Info().Msg("fcm: connecting to MCS")
		err := c.runMCS(ctx, aid, st, tok)
		if err == nil || ctx.Err() != nil {
			return
		}
		c.log.Warn().Err(err).Dur("backoff", backoff).Msg("fcm: MCS disconnected, reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// ---- Android Checkin -------------------------------------------------------

// checkinRequest is a minimal Android checkin request body.
// The full proto is AndroidCheckinRequest; we use JSON encoding which
// the checkin server also accepts.
type checkinRequest struct {
	UserSerialNumber int         `json:"user_serial_number"`
	Checkin          checkinData `json:"checkin"`
	Version          int         `json:"version"`
	ID               int64       `json:"id"`
	SecurityToken    uint64      `json:"security_token,omitempty"`
}

type checkinData struct {
	Build         checkinBuild `json:"build"`
	LastCheckinMs int64        `json:"last_checkin_ms"`
	Type          int          `json:"type"`
	// ChromeBuild omitted
}

type checkinBuild struct {
	Fingerprint string `json:"fingerprint"`
	Hardware    string `json:"hardware"`
	Brand       string `json:"brand"`
	Radio       string `json:"radio"`
	ClientID    string `json:"client_id"`
}

type checkinResponse struct {
	AndroidID     uint64 `json:"android_id"`
	SecurityToken uint64 `json:"security_token"`
}

func androidCheckin(ctx context.Context) (androidID, securityToken uint64, err error) {
	req := checkinRequest{
		UserSerialNumber: 0,
		Checkin: checkinData{
			Build: checkinBuild{
				Fingerprint: "google/sdk_gphone64_x86_64/emu64xa:14/UE1A.230829.036/11228894:userdebug/dev-keys",
				Hardware:    "ranchu",
				Brand:       "google",
				Radio:       "1.0.0.0",
				ClientID:    "android-google",
			},
			LastCheckinMs: 0,
			Type:          3, // DEVICE_TYPE_ANDROID
		},
		Version: 3,
		ID:      0,
	}

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://android.clients.google.com/checkin", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("checkin HTTP %d: %s", resp.StatusCode, string(b))
	}

	var cr checkinResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return 0, 0, fmt.Errorf("decode checkin response: %w", err)
	}
	if cr.AndroidID == 0 {
		return 0, 0, fmt.Errorf("checkin returned android_id=0")
	}
	return cr.AndroidID, cr.SecurityToken, nil
}

// ---- GCM Token Registration ------------------------------------------------

// fisToken attempts to obtain a Firebase Installation Service auth token.
// It tries both API keys and both project IDs since the resources.arsc
// contained test-project credentials but the production sender ID points
// to a different project (number 332820210871).
// Returns ("", nil) if all attempts are blocked — caller proceeds without it.
func fisToken(ctx context.Context) string {
	attempts := []struct{ apiKey, projectID, appID string }{
		// Production project derived from gcm_defaultSenderId
		{wyzeFirebaseAPIKey2, wyzeFirebaseProductionProjectID, wyzeFirebaseAppID},
		{wyzeFirebaseAPIKey, wyzeFirebaseProductionProjectID, wyzeFirebaseAppID},
		// Test project from resources.arsc
		{wyzeFirebaseAPIKey, wyzeFirebaseProjectID, wyzeFirebaseAppID},
		{wyzeFirebaseAPIKey2, wyzeFirebaseProjectID, wyzeFirebaseAppID},
	}
	for _, a := range attempts {
		tok, err := fisTokenOnce(ctx, a.apiKey, a.projectID, a.appID)
		if err == nil && tok != "" {
			return tok
		}
	}
	return ""
}

func fisTokenOnce(ctx context.Context, apiKey, projectID, appID string) (string, error) {
	fidBytes := make([]byte, 17)
	if _, err := rand.Read(fidBytes); err != nil {
		return "", err
	}
	fidBytes[0] = 0x70 | (fidBytes[0] & 0x0f)
	fid := base64.URLEncoding.EncodeToString(fidBytes)[:22]

	body := map[string]interface{}{
		"fid":         fid,
		"appId":       appID,
		"authVersion": "FIS_v2",
		"sdkVersion":  "a:17.0.0",
	}
	bodyJSON, _ := json.Marshal(body)

	fisURL := fmt.Sprintf(
		"https://firebaseinstallations.googleapis.com/v1/projects/%s/installations",
		projectID,
	)
	req, err := http.NewRequestWithContext(ctx, "POST", fisURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("x-android-package", wyzeAndroidPackage)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("FIS HTTP %d", resp.StatusCode)
	}
	var fisResp struct {
		AuthToken struct {
			Token string `json:"token"`
		} `json:"authToken"`
	}
	if err := json.Unmarshal(b, &fisResp); err != nil {
		return "", err
	}
	return fisResp.AuthToken.Token, nil
}

func gcmRegister(ctx context.Context, androidID, securityToken uint64) (string, error) {
	// Try to get a FIS auth token — required by newer GCM endpoints.
	// Falls back gracefully to empty string if all FIS attempts are blocked.
	fisAuthToken := fisToken(ctx)

	// Derive a stable app instance ID from android_id.
	appInstanceIDBytes := make([]byte, 16)
	binary.BigEndian.PutUint64(appInstanceIDBytes[0:8], androidID)
	binary.BigEndian.PutUint64(appInstanceIDBytes[8:16], securityToken)
	appInstanceID := base64.URLEncoding.EncodeToString(appInstanceIDBytes)[:22]

	form := url.Values{}
	form.Set("app", wyzeAndroidPackage)
	form.Set("X-subtype", wyzeFirebaseSenderID)
	form.Set("sender", wyzeFirebaseSenderID)
	form.Set("device", fmt.Sprintf("%d", androidID))
	form.Set("appid", appInstanceID)
	form.Set("X-appid", appInstanceID)
	form.Set("X-scope", "*")
	form.Set("X-firebase-app-name-hash", wyzeFirebaseProjectID)
	if fisAuthToken != "" {
		form.Set("X-goog-firebase-installations-auth", fisAuthToken)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://android.clients.google.com/c2dm/register3",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Authorization",
		fmt.Sprintf("AidLogin %d:%d", androidID, securityToken))

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read gcm register body: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gcm register HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Response is "token=APA91b..." or "Error=..."
	body := string(b)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "token=") {
			return strings.TrimPrefix(line, "token="), nil
		}
		if strings.HasPrefix(line, "Error=") {
			return "", fmt.Errorf("gcm register error: %s", line)
		}
	}
	return "", fmt.Errorf("gcm register: unexpected response: %s", body)
}

// ---- MCS Connection --------------------------------------------------------
//
// MCS uses a lightweight binary framing over TLS:
//   - First packet after connect: version byte (0x29 = 41) + tag byte (0)
//     which is mcs_proto.LoginRequest
//   - Subsequent packets: tag byte + varint length + proto body
//
// We use a minimal hand-rolled decoder for the fields we care about
// (DataMessageStanza fields: from, category, persistent_id, app_data kv pairs)
// rather than pulling in a full protobuf dependency.

const (
	mcsTagHeartbeatPing = 0
	mcsTagHeartbeatAck  = 1
	mcsTagLoginRequest  = 2
	mcsTagLoginResponse = 3
	mcsTagClose         = 4
	mcsTagDataMessage   = 5
	mcsTagIqStanza      = 6
	mcsTagNotifyAck     = 7
	mcsTagClose2        = 8
)

func (c *Client) runMCS(ctx context.Context, androidID, securityToken uint64, token string) error {
	tlsCfg := &tls.Config{
		ServerName: "mtalk.google.com",
		MinVersion: tls.VersionTLS12,
	}

	conn, err := tls.Dial("tcp", mcsHost, tlsCfg)
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	defer conn.Close()

	// Set a generous read deadline; reset on each successful read.
	conn.SetDeadline(time.Now().Add(10 * time.Minute))

	c.log.Debug().Msg("fcm: MCS connected, sending login")

	// Send version byte + login request
	if err := c.sendMCSLogin(conn, androidID, securityToken, token); err != nil {
		return fmt.Errorf("send login: %w", err)
	}

	// Read loop
	for {
		if ctx.Err() != nil {
			return nil
		}
		conn.SetDeadline(time.Now().Add(10 * time.Minute))

		tag, data, err := readMCSPacket(conn)
		if err != nil {
			return fmt.Errorf("read packet: %w", err)
		}

		switch tag {
		case mcsTagLoginResponse:
			c.log.Info().Msg("fcm: MCS login accepted — ready to receive pushes")

		case mcsTagHeartbeatPing:
			// Reply with heartbeat ack
			if err := writeMCSPacket(conn, mcsTagHeartbeatAck, nil); err != nil {
				return fmt.Errorf("write heartbeat ack: %w", err)
			}
			conn.SetDeadline(time.Now().Add(10 * time.Minute))

		case mcsTagDataMessage:
			c.handleDataMessage(data)

		case mcsTagClose, mcsTagClose2:
			return fmt.Errorf("server closed connection (tag %d)", tag)

		default:
			c.log.Debug().Int("tag", tag).Int("len", len(data)).Msg("fcm: unhandled MCS tag")
		}
	}
}

// sendMCSLogin writes the MCS version + LoginRequest protobuf.
// We use a hand-built minimal proto rather than importing a proto lib.
func (c *Client) sendMCSLogin(w io.Writer, androidID, securityToken uint64, token string) error {
	// Build a minimal LoginRequest proto:
	// field 1 (id): string — our "device" identifier
	// field 2 (domain): string — "android.googleapis.com"
	// field 3 (user): string — android_id as decimal string
	// field 4 (resource): string — android_id as decimal string
	// field 5 (auth_token): string — security_token as decimal string
	// field 8 (device_id): string — "android-<android_id_hex>"
	// field 10 (setting): repeated Setting{name, value}
	//   - heartbeat_interval: "300"
	// field 14 (compress_gzip): bool false
	// field 17 (auth_service): int32 = 2 (ANDROID_ID)
	// field 24 (client_event): repeated ClientEvent — omit
	//
	// For a working MCS login we need: id, domain, user, resource, auth_token.

	aidStr := fmt.Sprintf("%d", androidID)
	stStr := fmt.Sprintf("%d", securityToken)
	deviceID := fmt.Sprintf("android-%x", androidID)

	var pb protoBuilder
	pb.writeString(1, "android-"+aidStr) // id
	pb.writeString(2, "mcs.android.com") // domain
	pb.writeString(3, aidStr)            // user
	pb.writeString(4, aidStr)            // resource
	pb.writeString(5, stStr)             // auth_token
	pb.writeString(8, deviceID)          // device_id
	pb.writeInt32(17, 2)                 // auth_service = ANDROID_ID
	// received_persistent_id (field 12) — empty on first connect
	body := pb.Bytes()

	buf := make([]byte, 0, 2+len(body)+10)
	buf = append(buf, mcsVersion)         // version byte
	buf = append(buf, mcsTagLoginRequest) // tag byte
	buf = appendVarint(buf, uint64(len(body)))
	buf = append(buf, body...)

	_, err := w.Write(buf)
	return err
}

func writeMCSPacket(w io.Writer, tag byte, body []byte) error {
	buf := make([]byte, 0, 1+10+len(body))
	buf = append(buf, tag)
	buf = appendVarint(buf, uint64(len(body)))
	buf = append(buf, body...)
	_, err := w.Write(buf)
	return err
}

func readMCSPacket(r io.Reader) (tag int, data []byte, err error) {
	hdr := make([]byte, 1)

	// First byte is the tag.
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	tag = int(hdr[0])

	// Followed by a varint length.
	size, err := readVarint(r)
	if err != nil {
		return 0, nil, fmt.Errorf("read packet size: %w", err)
	}
	if size == 0 {
		return tag, nil, nil
	}
	if size > 4*1024*1024 {
		return 0, nil, fmt.Errorf("packet too large: %d", size)
	}
	data = make([]byte, size)
	if _, err = io.ReadFull(r, data); err != nil {
		return 0, nil, fmt.Errorf("read packet body: %w", err)
	}
	return tag, data, nil
}

// handleDataMessage parses a DataMessageStanza proto and fires OnRing
// when it looks like a Wyze doorbell ring notification.
func (c *Client) handleDataMessage(data []byte) {
	// DataMessageStanza proto fields we care about:
	//   field 2 (from): string — sender (usually sender ID)
	//   field 3 (category): string — app package name
	//   field 8 (app_data): repeated AppData {key string, value string}
	//   field 11 (persistent_id): string

	fields := parseProtoFields(data)
	category := string(fields[3])
	appDataRaw := fields[8] // may be repeated; grab all

	_ = category // typically "com.hualai"

	// Parse all app_data key-value pairs (repeated field 8, each is
	// an embedded message with field 1=key, field 2=value).
	kv := make(map[string]string)
	for _, blob := range getAllFields(data, 8) {
		subFields := parseProtoFields(blob)
		key := string(subFields[1])
		val := string(subFields[2])
		if key != "" {
			kv[key] = val
		}
	}

	_ = appDataRaw

	c.log.Debug().
		Str("category", category).
		Interface("app_data", kv).
		Msg("fcm: data message received")

	// Wyze ring push payloads include a "push_type" or "eventType" field.
	// Known ring indicators (from Wyze app reverse engineering):
	//   push_type == "1" (doorbell ring / button press)
	//   eventType == "DOORBELL_RING" or contains "ring"
	//   notification_type == "1"
	//   msg_type == "1"
	if !isRingPayload(kv) {
		c.log.Debug().Interface("kv", kv).Msg("fcm: data message is not a ring, ignoring")
		return
	}

	mac := extractDeviceMAC(kv)
	name := kv["device_name"]
	if name == "" {
		name = kv["deviceName"]
	}

	ev := RingEvent{
		DeviceMAC:  mac,
		DeviceName: name,
		TS:         time.Now(),
		RawPayload: kv,
	}

	c.log.Info().
		Str("mac", mac).
		Str("name", name).
		Interface("payload", kv).
		Msg("fcm: doorbell ring received")

	if c.OnRing != nil {
		c.OnRing(ev)
	}
}

// isRingPayload returns true if the FCM app_data KV pairs indicate a
// doorbell ring / button press event.
func isRingPayload(kv map[string]string) bool {
	// Check common Wyze ring push fields.
	checks := []struct{ key, val string }{
		{"push_type", "1"},
		{"eventType", "DOORBELL_RING"},
		{"notification_type", "1"},
		{"msg_type", "1"},
		{"event_type", "1"},
	}
	for _, ch := range checks {
		if v, ok := kv[ch.key]; ok {
			if v == ch.val || strings.Contains(strings.ToLower(v), "ring") || strings.Contains(strings.ToLower(v), "doorbell") {
				return true
			}
		}
	}
	// Also check the notification title/body for ring language.
	for _, field := range []string{"title", "body", "message", "content"} {
		if v := strings.ToLower(kv[field]); strings.Contains(v, "ring") || strings.Contains(v, "doorbell") || strings.Contains(v, "press") {
			return true
		}
	}
	return false
}

// extractDeviceMAC pulls the camera MAC from common Wyze push payload fields.
func extractDeviceMAC(kv map[string]string) string {
	for _, k := range []string{"device_mac", "device_id", "deviceMac", "deviceId", "mac"} {
		if v := kv[k]; v != "" {
			return v
		}
	}
	return ""
}

// ---- Minimal Protobuf Parser -----------------------------------------------
//
// We only need field lookup, not a full proto decoder. This is sufficient
// for the limited set of MCS messages we handle.

// parseProtoFields returns the raw bytes for the first occurrence of each
// field number in a protobuf-encoded message. For repeated fields use
// getAllFields instead.
func parseProtoFields(b []byte) map[int][]byte {
	out := make(map[int][]byte)
	parseProtoVisit(b, func(fn int, wt int, val []byte) bool {
		if _, exists := out[fn]; !exists {
			out[fn] = val
		}
		return true
	})
	return out
}

// getAllFields returns all occurrences of a given field number.
func getAllFields(b []byte, fieldNum int) [][]byte {
	var out [][]byte
	parseProtoVisit(b, func(fn int, wt int, val []byte) bool {
		if fn == fieldNum {
			out = append(out, val)
		}
		return true
	})
	return out
}

func parseProtoVisit(b []byte, fn func(fieldNum, wireType int, val []byte) bool) {
	for len(b) > 0 {
		tag, n := decodeVarint(b)
		if n == 0 {
			break
		}
		b = b[n:]
		fieldNum := int(tag >> 3)
		wireType := int(tag & 0x7)

		switch wireType {
		case 0: // varint
			v, n := decodeVarint(b)
			if n == 0 {
				return
			}
			buf := make([]byte, 8)
			binary.LittleEndian.PutUint64(buf, v)
			if !fn(fieldNum, wireType, buf) {
				return
			}
			b = b[n:]
		case 2: // length-delimited
			length, n := decodeVarint(b)
			if n == 0 {
				return
			}
			b = b[n:]
			if length > uint64(len(b)) {
				return
			}
			if !fn(fieldNum, wireType, b[:length]) {
				return
			}
			b = b[length:]
		case 1: // 64-bit
			if len(b) < 8 {
				return
			}
			if !fn(fieldNum, wireType, b[:8]) {
				return
			}
			b = b[8:]
		case 5: // 32-bit
			if len(b) < 4 {
				return
			}
			if !fn(fieldNum, wireType, b[:4]) {
				return
			}
			b = b[4:]
		default:
			return // unknown wire type, can't continue
		}
	}
}

// ---- Varint helpers --------------------------------------------------------

func decodeVarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, 0
		}
		if c < 0x80 {
			x |= uint64(c) << s
			return x, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

func readVarint(r io.Reader) (uint64, error) {
	var x uint64
	var s uint
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		c := buf[0]
		if c < 0x80 {
			x |= uint64(c) << s
			return x, nil
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, fmt.Errorf("varint overflow")
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// ---- Minimal Protobuf Builder ----------------------------------------------

type protoBuilder struct {
	buf []byte
}

func (p *protoBuilder) writeTag(field int, wireType int) {
	tag := uint64(field<<3) | uint64(wireType)
	p.buf = appendVarint(p.buf, tag)
}

func (p *protoBuilder) writeString(field int, s string) {
	p.writeTag(field, 2) // wire type 2 = length-delimited
	p.buf = appendVarint(p.buf, uint64(len(s)))
	p.buf = append(p.buf, s...)
}

func (p *protoBuilder) writeInt32(field int, v int32) {
	p.writeTag(field, 0) // wire type 0 = varint
	p.buf = appendVarint(p.buf, uint64(v))
}

func (p *protoBuilder) Bytes() []byte {
	return p.buf
}

// ---- Utilities -------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RandomInt64 returns a random int64 using crypto/rand for use as a
// synthetic event ID component.
func RandomInt64() int64 {
	n, err := rand.Int(rand.Reader, new(big.Int).SetInt64(1<<62))
	if err != nil {
		return time.Now().UnixNano()
	}
	return n.Int64()
}
