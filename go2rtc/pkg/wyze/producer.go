package wyze

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/tutk"
	"github.com/pion/rtp"
)

// ioctrlRingCandidates is the set of HL CommandIDs observed (or
// suspected) to carry a doorbell-ring notification from the camera.
//
// HL_DB2 (Doorbell v2) on current firmware emits CommandID 10020
// (0x2714) as its unsolicited press notification. This is confirmed
// by live capture in Phase 0 of the wyze-ring feature (see
// SPEC-live-ring-tutk.md). WYZEDB3 (Doorbell v1) uses the same value.
// Additional candidates are listed as comments below and will be
// filtered in by the classifier if confirmed by capture.
//
// The set is intentionally small and conservative: any unknown cmdID
// that arrives is logged as a debug line and ignored — the stream
// continues unaffected.
const (
	// KCmdDoorbellRing is the camera→client unsolicited notification
	// sent when a doorbell button is physically pressed (HL_DB2 /
	// WYZEDB3 on TransCode+DTLS firmware).
	KCmdDoorbellRing uint16 = 10020 // 0x2714

	// KCmdDoorbellRingAlt is an alternative value observed on some
	// older WYZEDB3 firmware. Include conservatively; confirm via
	// Phase 0 capture before relying on it.
	KCmdDoorbellRingAlt uint16 = 10032 // 0x2720
)

// wyzeNotifyLine is the JSON wire format emitted to stdout.
// The bridge's RingWatcher parses this prefix + JSON.
type wyzeNotifyLine struct {
	Stream string `json:"stream"`
	MAC    string `json:"mac"`
	Kind   string `json:"kind"`
	TS     int64  `json:"ts"` // epoch milliseconds
}

// isRingCmdID returns true for HL CommandIDs that represent a
// physical doorbell button press. Fail-safe: unknown IDs return false
// and are logged without affecting the stream.
func isRingCmdID(cmdID uint16) bool {
	return cmdID == KCmdDoorbellRing || cmdID == KCmdDoorbellRingAlt
}

// emitRingNotify prints the structured WYZE-NOTIFY line to stdout,
// unconditionally (not via go2rtc's leveled logger) so it survives
// log.level=warn. The bridge's go2rtcmgr.Manager.emitLogLine intercepts
// this prefix before any level mapping.
func emitRingNotify(streamName, mac string) {
	line := wyzeNotifyLine{
		Stream: streamName,
		MAC:    mac,
		Kind:   "ring",
		TS:     time.Now().UnixMilli(),
	}
	b, _ := json.Marshal(line)
	fmt.Println("WYZE-NOTIFY " + string(b))
}

type Producer struct {
	core.Connection
	client *Client
	model  string
}

func NewProducer(rawURL string) (*Producer, error) {
	client, err := Dial(rawURL)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(rawURL)
	query := u.Query()

	// 0 = HD (default), 1 = SD/360P, 2 = 720P, 3 = 2K, 4 = Floodlight
	var quality byte
	switch s := query.Get("subtype"); s {
	case "", "hd":
		quality = 0
	case "sd":
		quality = FrameSize360P
	default:
		quality = core.ParseByte(s)
	}

	medias, err := probe(client, quality)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	// Derive the stream name from the URL so the WYZE-NOTIFY line
	// carries a human-readable identifier the bridge can match to a
	// camera. The bridge sets the go2rtc stream name as the normalized
	// camera name; it isn't in the wyze:// URL directly, so we use the
	// MAC (always present) as a stable fallback. The bridge resolves
	// MAC → camera name on its side.
	mac := query.Get("mac")

	// Install the ring notification watcher when &notify=true is set.
	// This is gated so the behavior is identical to upstream go2rtc when
	// the flag is absent — no WYZE-NOTIFY output, no extra goroutines.
	if client.Notify() && client.conn != nil {
		client.conn.NotifyFunc = func(cmdID uint16, hlPayload []byte) {
			// Always emit every unsolicited IOCTRL frame unconditionally
			// so the bridge can capture the real CommandID from a live press.
			// Once the ring CommandID is confirmed, isRingCmdID can be
			// tightened to filter only that value.
			if isRingCmdID(cmdID) {
				emitRingNotify("", mac)
			}
			// Always dump unknown frames too — this is the Phase 0 capture path.
			// The bridge logs these at INFO so they're visible at default log level.
			// Format: WYZE-IOCTRL <cmdID_decimal> <cmdID_hex> <payloadLen> <payload_hex>
			payloadPreview := hlPayload
			if len(payloadPreview) > 32 {
				payloadPreview = payloadPreview[:32]
			}
			fmt.Printf("WYZE-IOCTRL cmdID=%d (0x%04x) payloadLen=%d payload=%x\n",
				cmdID, cmdID, len(hlPayload), payloadPreview)
		}
	}

	prod := &Producer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "wyze",
			Protocol:   client.Protocol(),
			RemoteAddr: client.RemoteAddr().String(),
			Source:     rawURL,
			Medias:     medias,
			Transport:  client,
		},
		client: client,
		model:  query.Get("model"),
	}

	return prod, nil
}

func (p *Producer) Start() error {
	for {
		if p.client.verbose {
			fmt.Println("[Wyze] Reading packet...")
		}

		_ = p.client.SetDeadline(time.Now().Add(core.ConnDeadline))
		pkt, err := p.client.ReadPacket()
		if err != nil {
			return err
		}
		if pkt == nil {
			continue
		}

		var name string
		var pkt2 *core.Packet

		switch codecID := pkt.Codec; codecID {
		case tutk.CodecH264:
			name = core.CodecH264
			pkt2 = &core.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: annexb.EncodeToAVCC(pkt.Payload),
			}

		case tutk.CodecH265:
			name = core.CodecH265
			pkt2 = &core.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: annexb.EncodeToAVCC(pkt.Payload),
			}

		case tutk.CodecPCMU:
			name = core.CodecPCMU
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		case tutk.CodecPCMA:
			name = core.CodecPCMA
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		case tutk.CodecAACADTS, tutk.CodecAACAlt, tutk.CodecAACRaw, tutk.CodecAACLATM:
			name = core.CodecAAC
			payload := pkt.Payload
			if aac.IsADTS(payload) {
				payload = payload[aac.ADTSHeaderLen(payload):]
			}
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: aac.RTPPacketVersionAAC, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: payload,
			}

		case tutk.CodecOpus:
			name = core.CodecOpus
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		case tutk.CodecPCML:
			name = core.CodecPCML
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		case tutk.CodecMP3:
			name = core.CodecMP3
			pkt2 = &core.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		case tutk.CodecMJPEG:
			name = core.CodecJPEG
			pkt2 = &core.Packet{
				Header:  rtp.Header{SequenceNumber: uint16(pkt.FrameNo), Timestamp: pkt.Timestamp},
				Payload: pkt.Payload,
			}

		default:
			continue
		}

		for _, recv := range p.Receivers {
			if recv.Codec.Name == name {
				recv.WriteRTP(pkt2)
				break
			}
		}
	}
}

func probe(client *Client, quality byte) ([]*core.Media, error) {
	client.SetResolution(quality)
	client.SetDeadline(time.Now().Add(core.ProbeTimeout))

	var vcodec, acodec *core.Codec
	var tutkAudioCodec byte

	for {
		if client.verbose {
			fmt.Println("[Wyze] Probing for codecs...")
		}

		pkt, err := client.ReadPacket()
		if err != nil {
			return nil, fmt.Errorf("wyze: probe: %w", err)
		}
		if pkt == nil || len(pkt.Payload) < 5 {
			continue
		}

		switch pkt.Codec {
		case tutk.CodecH264:
			if vcodec == nil {
				buf := annexb.EncodeToAVCC(pkt.Payload)
				if len(buf) >= 5 && h264.NALUType(buf) == h264.NALUTypeSPS {
					vcodec = h264.AVCCToCodec(buf)
				}
			}
		case tutk.CodecH265:
			if vcodec == nil {
				buf := annexb.EncodeToAVCC(pkt.Payload)
				if len(buf) >= 5 && h265.NALUType(buf) == h265.NALUTypeVPS {
					vcodec = h265.AVCCToCodec(buf)
				}
			}
		case tutk.CodecPCMU:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecPCMU, ClockRate: pkt.SampleRate, Channels: pkt.Channels}
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecPCMA:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecPCMA, ClockRate: pkt.SampleRate, Channels: pkt.Channels}
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecAACAlt, tutk.CodecAACADTS, tutk.CodecAACRaw, tutk.CodecAACLATM:
			if acodec == nil {
				config := aac.EncodeConfig(aac.TypeAACLC, pkt.SampleRate, pkt.Channels, false)
				acodec = aac.ConfigToCodec(config)
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecOpus:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecPCML:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecPCML, ClockRate: pkt.SampleRate, Channels: pkt.Channels}
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecMP3:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecMP3, ClockRate: pkt.SampleRate, Channels: pkt.Channels}
				tutkAudioCodec = pkt.Codec
			}
		case tutk.CodecMJPEG:
			if vcodec == nil {
				vcodec = &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
			}
		}

		if vcodec != nil && (acodec != nil || !client.SupportsAudio()) {
			break
		}
	}

	_ = client.SetDeadline(time.Time{})

	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{vcodec},
		},
	}

	if acodec != nil {
		medias = append(medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{acodec},
		})

		if client.SupportsIntercom() {
			client.SetBackchannelCodec(tutkAudioCodec, acodec.ClockRate, uint8(acodec.Channels))
			medias = append(medias, &core.Media{
				Kind:      core.KindAudio,
				Direction: core.DirectionSendonly,
				Codecs:    []*core.Codec{acodec.Clone()},
			})
		}
	}

	if client.verbose {
		fmt.Printf("[Wyze] Probed codecs: video=%s audio=%s\n", vcodec.Name, acodec.Name)
		if client.SupportsIntercom() {
			fmt.Printf("[Wyze] Intercom supported, audio send codec=%s\n", acodec.Name)
		}
	}

	return medias, nil
}
