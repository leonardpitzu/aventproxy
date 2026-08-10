package rtsp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// h264ClockRate is the RTP timestamp rate H.264 always uses.
const h264ClockRate = 90000

// cameraSkill parses a camera's stored skill; nil means "use the defaults".
func cameraSkill(camera *storage.CameraInfo) *tuya.Skill {
	if camera == nil || camera.Skill == "" {
		return nil
	}
	var skill *tuya.Skill
	if err := json.Unmarshal([]byte(camera.Skill), &skill); err != nil {
		core.Logger.Warn().Err(err).Msg("Could not parse skill, using defaults")
		return nil
	}
	return skill
}

// audioRTPProfile maps the monitor's advertised audio codec to the static RTP
// payload type and rtpmap name. Tuya's ids: 101 and 105 are u-law, 106 A-law.
// The LAN path has to answer this the same way the RTSP description does, or a
// client decodes the samples with the wrong companding.
func audioRTPProfile(skill *tuya.Skill) (uint8, string) {
	if skill != nil && len(skill.Audios) > 0 && skill.Audios[0].CodecType == 106 {
		return 8, "PCMA/8000"
	}
	return 0, "PCMU/8000"
}

// LANBridge streams a monitor over the local network instead of the cloud.
//
// It deliberately shares the caller's RTPForwarder: everything downstream —
// RTSP clients, SPS/PPS caching for late joiners, TCP interleaving — is
// transport-agnostic, so the LAN path only has to end at ForwardVideoPacket
// and ForwardAudioPacket.
type LANBridge struct {
	camera    *storage.CameraInfo
	forwarder *RTPForwarder

	client    *lan.Client
	ssrc      uint32
	seq       atomic.Uint32
	audioSSRC uint32
	audioSeq  atomic.Uint32
	audioPT   uint8
	audio     g711Encoder
	audioWarn sync.Once
	framing   framingProbe
	payloader codecs.H264Payloader
	ctx       context.Context
	cancel    context.CancelFunc

	mu        sync.Mutex
	connected bool
}

// NewLANBridge prepares a LAN bridge feeding an existing forwarder.
func NewLANBridge(camera *storage.CameraInfo, forwarder *RTPForwarder) *LANBridge {
	// The LAN protocol can only be diagnosed against real hardware, so its
	// tracing follows the bridge's own log level.
	lan.Debugf = func(format string, args ...any) {
		core.Logger.Debug().Msgf(format, args...)
	}
	return &LANBridge{camera: camera, forwarder: forwarder}
}

// Possible returns whether this camera has the credentials the LAN path needs.
func (lb *LANBridge) Possible() bool {
	return lb.camera != nil && lb.camera.LocalKey != "" && lb.camera.Password != ""
}

// Start negotiates a local session and begins forwarding video.
func (lb *LANBridge) Start() error {
	lb.mu.Lock()
	if lb.connected {
		lb.mu.Unlock()
		return errors.New("lan bridge already connected")
	}
	lb.mu.Unlock()

	if !lb.Possible() {
		return errors.New("lan bridge needs a localKey and password")
	}

	// A fresh SSRC per session keeps a reconnect from looking like a
	// continuation of the previous stream to RTSP clients.
	lb.ssrc = randomSSRC()
	lb.audioSSRC = randomSSRC()
	lb.audioPT, _ = audioRTPProfile(cameraSkill(lb.camera))
	lb.audio.alaw = lb.audioPT == 8

	lb.ctx, lb.cancel = context.WithCancel(context.Background())
	lb.client = lan.NewClient(
		lb.camera.DeviceID,
		lb.camera.LocalKey,
		lb.camera.Password,
		lb.camera.UID,
		lb.camera.LanIP,
		lb.onFrame,
		lb.onAudio,
	)

	if err := lb.client.Start(lb.ctx); err != nil {
		// Start binds a UDP socket before it can fail, so the client owns an fd
		// even on the error path and the cloud fallback would leak one per try.
		lb.client.Close()
		lb.client = nil
		lb.cancel()
		return err
	}

	lb.mu.Lock()
	lb.connected = true
	lb.mu.Unlock()
	core.Logger.Info().Msgf("LAN stream established for camera %s", lb.camera.DeviceName)
	return nil
}

// rtpMTU is the largest payload put in one packet. A datagram bigger than the
// path allows is refused outright ("message too long"), and a whole NAL can
// exceed it now that the pieces of one are joined before forwarding.
const rtpMTU = 1200

// onFrame hands one of the monitor's RTP payloads to the shared forwarder.
//
// The monitor emits RTP payloads rather than raw NALs, so they normally need
// only a header: re-packetising an FU-A fragment would wrap one encapsulation
// in another and produce a stream that flows but decodes to nothing. A whole
// NAL too big for one packet is the exception, and only that gets fragmented.
func (lb *LANBridge) onFrame(frame *lan.VideoFrame) {
	if frame == nil || len(frame.NAL) < 2 {
		return
	}
	lb.framing.observe(frame, lb.camera.DeviceName)

	timestamp := uint32(frame.Timestamp * 9 / 100) // the monitor counts microseconds
	for _, payload := range lb.split(frame.NAL) {
		lb.forwarder.ForwardVideoPacket(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: uint16(lb.seq.Add(1)),
				Timestamp:      timestamp,
				SSRC:           lb.ssrc,
				Marker:         endsAccessUnit(payload),
			},
			Payload: payload,
		})
	}
}

// split fragments a payload only when it is a whole NAL that will not fit.
// Anything already fragmented is passed through untouched.
func (lb *LANBridge) split(payload []byte) [][]byte {
	if len(payload) <= rtpMTU {
		return [][]byte{payload}
	}
	if nalType := payload[0] & 0x1f; nalType >= 28 {
		// Already an aggregation or a fragment; re-wrapping it is the one thing
		// that must not happen here.
		return [][]byte{payload}
	}
	if out := lb.payloader.Payload(rtpMTU, payload); len(out) > 0 {
		return out
	}
	return [][]byte{payload}
}

// onAudio hands one of the monitor's audio units to the shared forwarder.
//
// The monitor sends 16 kHz linear PCM but the description promises G.711 at
// 8 kHz, so the unit is resampled and companded on the way through. The
// forwarder rebases the timestamp onto its own 8 kHz clock.
func (lb *LANBridge) onAudio(frame *lan.AudioFrame) {
	if frame == nil || len(frame.Samples) == 0 {
		return
	}
	if frame.SampleRate != lanAudioRate || frame.Channels != 1 {
		lb.audioWarn.Do(func() {
			core.Logger.Warn().Msgf(
				"Ignoring LAN audio from %s: %d Hz, %d channel(s), codec %d is not the 16 kHz mono this converts",
				lb.camera.DeviceName, frame.SampleRate, frame.Channels, frame.Codec)
		})
		return
	}
	payload := lb.audio.encode(frame.Samples)
	if len(payload) == 0 {
		return
	}
	lb.forwarder.ForwardAudioPacket(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    lb.audioPT,
			SequenceNumber: uint16(lb.audioSeq.Add(1)),
			SSRC:           lb.audioSSRC,
		},
		Payload: payload,
	})
}

// endsAccessUnit reports whether a payload completes a picture, which is what
// the RTP marker bit means for H.264.
func endsAccessUnit(payload []byte) bool {
	switch payload[0] & 0x1f {
	case 28, 29: // FU-A / FU-B: the end bit lives in the fragmentation header
		return payload[1]&0x40 != 0
	case 1, 5: // a whole slice, coded or IDR
		return true
	default: // parameter sets and SEI precede the picture they describe
		return false
	}
}

// Stop tears the session down.
func (lb *LANBridge) Stop() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if !lb.connected && lb.client == nil {
		return
	}
	if lb.cancel != nil {
		lb.cancel()
	}
	if lb.client != nil {
		lb.client.Close()
		lb.client = nil
	}
	lb.connected = false
}

func randomSSRC() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}
