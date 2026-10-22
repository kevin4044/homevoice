package broker

import (
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/homevoice/homevoice/internal/m0/config"
)

// identityHook is the single mochi-mqtt hook that enforces the M0 trust and
// authorization contract:
//
//   - authentication against the exactly pre-registered client_id, username
//     and password (constant-time comparisons, no payload identity);
//   - exact-topic ACLs for publish and subscribe;
//   - trusted uplink attribution (the authenticated connection, not payload);
//   - per-device connection generations so a replaced connection cannot clear
//     the new registration or inject messages into the new session;
//   - bound checks for QoS and packet size.
type identityHook struct {
	mqtt.HookBase
	b *Broker
}

// hookID is the stable identifier reported by mochi logs.
const hookID = "homevoice-m0-identity"

func (h *identityHook) ID() string { return hookID }

func (h *identityHook) Provides(b byte) bool {
	switch b {
	case mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnConnect,
		mqtt.OnSessionEstablish,
		mqtt.OnPublish,
		mqtt.OnDisconnect,
		mqtt.OnPacketRead:
		return true
	default:
		return false
	}
}

// OnConnect rejects MQTT protocol versions other than 3.1.1 (4) explicitly.
// The library only supports a minimum version, so version 5 has to be stopped
// here. A CONNACK with the failure code is sent before the connection closes.
func (h *identityHook) OnConnect(cl *mqtt.Client, pk packets.Packet) error {
	if pk.ProtocolVersion == mqttProtocolLevel311 {
		return nil
	}
	h.b.log.Warn("rejected unsupported mqtt protocol version",
		"protocol_version", pk.ProtocolVersion,
		"operator", "mqtt 3.1.1 only",
	)
	if h.b.server != nil {
		_ = h.b.server.SendConnack(cl, packets.ErrUnsupportedProtocolVersion, false, nil)
	}
	return packets.ErrUnsupportedProtocolVersion
}

// mqttProtocolLevel311 is the MQTT 3.1.1 protocol level byte.
const mqttProtocolLevel311 byte = 4

// OnConnectAuthenticate verifies the pre-registered identity. Missing,
// unknown or incorrect credentials are rejected without creating any
// registration. Nothing from the packet is logged: client ids, usernames and
// passwords may be credentials.
func (h *identityHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	device, reason := h.b.authenticate(pk)
	if reason != "" {
		h.b.log.Warn("mqtt authentication rejected",
			"listener", cl.Net.Listener,
			"remote", cl.Net.Remote,
			"reason", reason,
		)
		return false
	}
	h.b.log.Debug("mqtt authentication accepted",
		"device", device.DeviceID,
		"listener", cl.Net.Listener,
	)
	return true
}

// OnACLCheck enforces exact-topic permissions: a device may publish only its
// own PublishTopic and subscribe only its own SubscribeTopic. Wildcards,
// other-device topics, admin topics, $SYS topics and unknown topics are all
// denied by exact equality.
func (h *identityHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if cl == nil {
		return false
	}
	if cl.Net.Inline {
		// Inline publishes come from the broker itself (downlink), not from
		// an untrusted MQTT client.
		return true
	}
	device, ok := h.b.deviceForClient(cl)
	if !ok {
		h.b.log.Warn("mqtt acl denied: unauthenticated connection", "listener", cl.Net.Listener)
		return false
	}
	if !validExactTopic(topic) {
		h.b.log.Warn("mqtt acl denied: wildcard/invalid topic",
			"device", device.DeviceID, "write", write, "topic", topic)
		return false
	}
	allowed := device.PublishTopic
	if !write {
		allowed = device.SubscribeTopic
	}
	if topic != allowed {
		h.b.log.Warn("mqtt acl denied: topic outside device grant",
			"device", device.DeviceID, "write", write, "topic", topic)
		return false
	}
	return true
}

// OnSessionEstablish registers the authenticated session and bumps the
// device's connection generation. It runs before mochi performs duplicate
// client-id takeover, so the replacement registration is already in place when
// the old connection's OnDisconnect fires.
func (h *identityHook) OnSessionEstablish(cl *mqtt.Client, _ packets.Packet) {
	device, ok := h.b.deviceForClient(cl)
	if !ok {
		return
	}
	h.b.registerSession(cl, device)
}

// OnPacketRead rejects QoS2 publishes (the probe does not implement QoS2
// flows) and packets exceeding the configured maximum before they are
// processed. Both cases close the connection through the normal error path;
// nothing is stored or forwarded.
func (h *identityHook) OnPacketRead(_ *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if pk.FixedHeader.Type == packets.Publish && pk.FixedHeader.Qos == 2 {
		h.b.log.Warn("rejected qos2 publish", "operator", "qos0/qos1 only")
		// mochi only propagates ErrRejectPacket from the read hook; any other
		// hook error is swallowed. ErrRejectPacket here closes the connection
		// before processing, without a PUBREC or forwarding.
		return pk, packets.ErrRejectPacket
	}
	if size := packetWireSize(pk.FixedHeader); size > int(h.b.opts.MaxPacketBytes) {
		h.b.log.Warn("rejected oversized mqtt packet",
			"bytes", size, "limit", h.b.opts.MaxPacketBytes)
		return pk, packets.ErrRejectPacket
	}
	return pk, nil
}

// OnPublish attributes an uplink message to the authenticated connection. It
// never reads identity from the payload. Messages from a superseded connection
// are dropped, retained control messages are rejected (and the offending
// connection closed), and the callback is invoked without holding broker locks.
func (h *identityHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl == nil || cl.Net.Inline {
		return pk, nil
	}
	device, ok := h.b.deviceForClient(cl)
	if !ok {
		return pk, packets.ErrRejectPacket
	}
	generation, current := h.b.currentGeneration(device.DeviceID, cl)
	if !current {
		h.b.log.Warn("dropped publish from superseded mqtt connection",
			"device", device.DeviceID, "generation", generation)
		return pk, packets.ErrRejectPacket
	}
	if pk.FixedHeader.Retain {
		h.b.log.Warn("rejected retained control publish",
			"device", device.DeviceID, "topic", pk.TopicName)
		// MQTT 3.1.1 has no negative PUBACK; close the connection so the
		// retained publish is explicitly not accepted and can never be
		// stored or replayed.
		cl.Stop(packets.ErrNotAuthorized)
		return pk, packets.ErrRejectPacket
	}
	if pk.TopicName != device.PublishTopic {
		// Defence in depth: the ACL already rejects this, but never forward
		// an uplink from a topic outside the device grant.
		h.b.log.Warn("dropped publish outside device topic",
			"device", device.DeviceID, "topic", pk.TopicName)
		return pk, packets.ErrRejectPacket
	}
	msg := Message{
		DeviceID:   device.DeviceID,
		Generation: generation,
		Topic:      pk.TopicName,
		Payload:    append([]byte(nil), pk.Payload...),
		QoS:        pk.FixedHeader.Qos,
	}
	h.b.log.Debug("mqtt uplink message",
		"device", msg.DeviceID,
		"generation", msg.Generation,
		"topic", msg.Topic,
		"bytes", len(msg.Payload),
		"qos", msg.QoS,
	)
	if h.b.opts.OnMessage != nil {
		// Documented synchronous callback: called from the connection
		// goroutine without broker locks held. Must not block or call Close.
		h.b.opts.OnMessage(msg)
	}
	return pk, nil
}

// OnDisconnect removes the session only if it still belongs to the closing
// connection. The disconnect of a superseded connection therefore cannot
// clear the replacement registration and emits no offline event for it.
func (h *identityHook) OnDisconnect(cl *mqtt.Client, err error, _ bool) {
	if cl == nil || cl.Net.Inline {
		return
	}
	device, ok := h.b.deviceForClient(cl)
	if !ok {
		return
	}
	h.b.removeSession(cl, device, h.b.disconnectReason(cl, err))
}

// disconnectReason classifies a disconnect into a small, secret-free reason.
func (b *Broker) disconnectReason(cl *mqtt.Client, err error) string {
	b.mu.Lock()
	forced, hasForced := b.forcedReasons[cl]
	if hasForced {
		delete(b.forcedReasons, cl)
	}
	b.mu.Unlock()
	if hasForced {
		return forced
	}
	if cl.IsTakenOver() {
		return reasonSessionTakenOver
	}
	if err == nil {
		if cause := cl.StopCause(); cause != nil {
			var code packets.Code
			if errors.As(cause, &code) {
				switch code {
				case packets.ErrNotAuthorized:
					return reasonNotAuthorized
				case packets.CodeDisconnect, packets.ErrSessionTakenOver:
					return reasonClientDisconnect
				}
				return reasonRejected
			}
		}
		return reasonClientDisconnect
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return reasonKeepaliveTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return reasonPeerClosed
	}
	return reasonConnectionError
}

// deviceForClient resolves the trusted device identity of an MQTT connection.
// The server only calls ACL/publish hooks for connections that passed
// OnConnectAuthenticate, and the username is the credential that was verified
// for this connection; the client id must still match the same registration.
func (b *Broker) deviceForClient(cl *mqtt.Client) (config.Device, bool) {
	if cl == nil {
		return config.Device{}, false
	}
	device, ok := b.byUsername[string(cl.Properties.Username)]
	if !ok || device.ClientID != cl.ID {
		return config.Device{}, false
	}
	return device, true
}

// authenticate performs the exact pre-registered credential check with
// constant-time comparisons. It returns a secret-free failure reason.
func (b *Broker) authenticate(pk packets.Packet) (config.Device, string) {
	if !pk.Connect.UsernameFlag || !pk.Connect.PasswordFlag ||
		len(pk.Connect.Username) == 0 || len(pk.Connect.Password) == 0 {
		return config.Device{}, "missing_credentials"
	}
	clientID := []byte(pk.Connect.ClientIdentifier)
	username := pk.Connect.Username
	password := pk.Connect.Password

	var matched config.Device
	matches := 0
	for _, device := range b.devices {
		idOK := subtle.ConstantTimeCompare([]byte(device.ClientID), clientID)
		userOK := subtle.ConstantTimeCompare([]byte(device.Username), username)
		passOK := subtle.ConstantTimeCompare([]byte(device.Password), password)
		if idOK&userOK&passOK == 1 {
			matched = device
			matches++
		}
	}
	switch {
	case matches == 0:
		return config.Device{}, "unknown_or_wrong_credentials"
	case matches > 1:
		// Configuration validation rejects ambiguous credentials; refuse
		// to guess if a caller bypassed it.
		return config.Device{}, "ambiguous_credentials"
	default:
		return matched, ""
	}
}

// validExactTopic reports whether topic is a concrete MQTT topic name that
// contains no filter wildcards and no NUL.
func validExactTopic(topic string) bool {
	if topic == "" || len(topic) > 65535 {
		return false
	}
	if strings.ContainsAny(topic, "+#\x00") {
		return false
	}
	return true
}

// packetWireSize returns the encoded size of a packet: fixed-header byte,
// variable-length remaining-length field and remaining length.
func packetWireSize(fh packets.FixedHeader) int {
	return 1 + remainingLengthFieldSize(fh.Remaining) + fh.Remaining
}

func remainingLengthFieldSize(remaining int) int {
	switch {
	case remaining < 128:
		return 1
	case remaining < 16_384:
		return 2
	case remaining < 2_097_152:
		return 3
	default:
		return 4
	}
}
