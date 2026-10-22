// Package limits holds the bounded-resource constants defined by the
// approved M0 specification (M0-v0.1, docs/milestones/M0-transport-probe).
//
// These are probe limits and tuning starting points. They are not validated
// firmware parameters, product SLAs or security guarantees. Changing a value
// requires a recorded decision by the specification owner; tests must still
// prove the behaviour is bounded.
package limits

import "time"

const (
	// MaxDevices is the maximum number of pre-registered probe identities.
	// Additional identities are rejected by authentication.
	MaxDevices = 2

	// MQTTConnectTimeout bounds how long the broker waits for a device to
	// finish the MQTT CONNECT handshake on a new TCP/TLS connection.
	MQTTConnectTimeout = 5 * time.Second

	// HelloWait bounds how long the probe waits for a HELLO after an
	// authenticated MQTT connection when a caller explicitly waits for one.
	// An idle MQTT connection is NOT required to send HELLO.
	HelloWait = 5 * time.Second

	// MaxMQTTPacketBytes is the largest MQTT packet the broker accepts or
	// the probe emits (64 KiB, including the fixed header).
	MaxMQTTPacketBytes = 64 * 1024

	// MaxJSONBytes is the largest application JSON body accepted in either
	// direction (64 KiB; must not exceed MaxMQTTPacketBytes).
	MaxJSONBytes = 64 * 1024

	// MaxUDPDatagramBytes is the application-level UDP datagram limit.
	// This is NOT a no-fragmentation guarantee: IP/UDP headers and tunnels
	// reduce the usable payload on a 1500-byte path MTU.
	MaxUDPDatagramBytes = 1500

	// UDPHeaderBytes is the cleartext UDP header length.
	UDPHeaderBytes = 16

	// MaxUDPPayloadBytes is the largest encrypted payload carried in one
	// probe UDP datagram.
	MaxUDPPayloadBytes = MaxUDPDatagramBytes - UDPHeaderBytes

	// AudioQueueFrames bounds the per-device outbound audio frame queue.
	// When full, frames are dropped and counted; allocation never grows
	// without bound.
	AudioQueueFrames = 64

	// ListenRoundMax bounds one listen round for auto/realtime modes.
	ListenRoundMax = 15 * time.Second

	// UpstreamOpMax bounds a single simulated upstream ASR/LLM/TTS call.
	UpstreamOpMax = 30 * time.Second

	// MCPRequestTimeout bounds one device MCP JSON-RPC request.
	MCPRequestTimeout = 5 * time.Second

	// MCPMaxPages bounds tools/list pagination.
	MCPMaxPages = 8

	// MCPMaxTools bounds the number of discovered device tools.
	MCPMaxTools = 128

	// ModelMaxRequests bounds model requests per agent turn.
	ModelMaxRequests = 2

	// ModelMaxToolCalls bounds tool executions per agent turn.
	ModelMaxToolCalls = 1

	// TTSSettle is the initial delay between MQTT tts start and the first
	// UDP audio frame (candidate; must be verified on hardware).
	TTSSettle = 150 * time.Millisecond

	// TTSPrebufferFrames is the initial number of frames buffered before
	// paced transmission starts (candidate).
	TTSPrebufferFrames = 5

	// TTSTailExtraFrames is the extra frames waited after the last frame
	// before MQTT tts stop (candidate).
	TTSTailExtraFrames = 2

	// Downlink* describe the candidate downlink audio format.
	DownlinkSampleRate      = 24000
	DownlinkChannels        = 1
	DownlinkFrameDurationMS = 60

	// Uplink* describe the reference uplink audio format.
	UplinkSampleRate      = 16000
	UplinkChannels        = 1
	UplinkFrameDurationMS = 60

	// UDPReplayWindow is the number of previous sequence numbers remembered
	// for inbound UDP replay rejection.
	UDPReplayWindow = 64

	// LiveMaxRounds is the maximum authorized cloud verification rounds.
	LiveMaxRounds = 3

	// LiveASRPerRound, LiveLLMPerRound and LiveTTSPerRound bound cloud calls
	// per authorized verification round.
	LiveASRPerRound = 1
	LiveLLMPerRound = 2
	LiveTTSPerRound = 1

	// OutboundSequenceMax is the largest outbound UDP sequence number the
	// session may emit. Before wrapping it must be re-established via HELLO.
	OutboundSequenceMax = uint32(1<<32 - 1)
)
