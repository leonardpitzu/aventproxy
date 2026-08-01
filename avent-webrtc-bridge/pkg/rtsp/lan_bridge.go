package rtsp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// h264ClockRate is the RTP timestamp rate H.264 always uses.
const h264ClockRate = 90000

// LANBridge streams a monitor over the local network instead of the cloud.
//
// It deliberately shares the caller's RTPForwarder: everything downstream —
// RTSP clients, SPS/PPS caching for late joiners, TCP interleaving — is
// transport-agnostic, so the LAN path only has to end at ForwardVideoPacket.
type LANBridge struct {
	camera    *storage.CameraInfo
	forwarder *RTPForwarder

	client     *lan.Client
	packetizer rtp.Packetizer
	ctx        context.Context
	cancel     context.CancelFunc

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
	lb.packetizer = rtp.NewPacketizer(
		1200,
		96,
		randomSSRC(),
		&codecs.H264Payloader{},
		rtp.NewRandomSequencer(),
		h264ClockRate,
	)

	lb.ctx, lb.cancel = context.WithCancel(context.Background())
	lb.client = lan.NewClient(
		lb.camera.DeviceID,
		lb.camera.LocalKey,
		lb.camera.Password,
		lb.camera.UID,
		lb.camera.LanIP,
		lb.onFrame,
	)

	if err := lb.client.Start(lb.ctx); err != nil {
		lb.cancel()
		return err
	}

	lb.mu.Lock()
	lb.connected = true
	lb.mu.Unlock()
	core.Logger.Info().Msgf("LAN stream established for camera %s", lb.camera.DeviceName)
	return nil
}

// onFrame packetises one NAL and hands it to the shared forwarder.
func (lb *LANBridge) onFrame(frame *lan.VideoFrame) {
	if frame == nil || len(frame.NAL) == 0 {
		return
	}
	// The monitor sends a 64-bit microsecond clock; RTP wants 90 kHz samples.
	samples := uint32(h264ClockRate / maxInt(frame.FPS, 1))
	for _, pkt := range lb.packetizer.Packetize(frame.NAL, samples) {
		lb.forwarder.ForwardVideoPacket(pkt)
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

// IsConnected reports whether video is flowing.
func (lb *LANBridge) IsConnected() bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.connected
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func randomSSRC() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}
