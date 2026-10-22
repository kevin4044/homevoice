package broker

// Shared acceptance-test helpers for the M0 MQTT transport probe. The tests
// use the real paho MQTT 3.1.1 client for normal flows and raw TCP for
// malformed/oversized/handshake-bounds cases.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
)

// ---------- recorder of broker callbacks ----------

type recorder struct {
	mu       sync.Mutex
	messages []Message
	offlines []OfflineEvent
}

func (r *recorder) onMessage(m Message) {
	m.Payload = append([]byte(nil), m.Payload...)
	r.mu.Lock()
	r.messages = append(r.messages, m)
	r.mu.Unlock()
}

func (r *recorder) onOffline(e OfflineEvent) {
	r.mu.Lock()
	r.offlines = append(r.offlines, e)
	r.mu.Unlock()
}

func (r *recorder) messagesSnapshot() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.messages...)
}

func (r *recorder) offlinesSnapshot() []OfflineEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]OfflineEvent(nil), r.offlines...)
}

func (r *recorder) waitMessage(t *testing.T, timeout time.Duration, pred func(Message) bool) Message {
	t.Helper()
	var found Message
	waitFor(t, timeout, fmt.Sprintf("callback message"), func() bool {
		for _, m := range r.messagesSnapshot() {
			if pred(m) {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

func (r *recorder) waitOffline(t *testing.T, timeout time.Duration, pred func(OfflineEvent) bool) OfflineEvent {
	t.Helper()
	var found OfflineEvent
	waitFor(t, timeout, "offline event", func() bool {
		for _, e := range r.offlinesSnapshot() {
			if pred(e) {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

// ---------- broker setup ----------

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultOffline()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

// startBroker starts a broker for the test and registers cleanup. Extra
// options are applied after the defaults so tests can shorten timeouts.
func startBroker(t *testing.T, cfg *config.Config, mutate func(*Options)) (*Broker, *recorder) {
	t.Helper()
	rec := &recorder{}
	opts := Options{
		BindAddr:       cfg.Network.MQTTBind,
		Devices:        cfg.Devices,
		Logger:         logging.Discard(),
		MaxPacketBytes: limits.MaxMQTTPacketBytes,
		ConnectTimeout: limits.MQTTConnectTimeout,
		OnMessage:      rec.onMessage,
		OnOffline:      rec.onOffline,
	}
	if mutate != nil {
		mutate(&opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatalf("New(broker): %v", err)
	}
	if err := b.Start(); err != nil {
		_ = b.Close()
		t.Fatalf("Start(broker): %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Close(broker): %v", err)
		}
	})
	return b, rec
}

// ---------- paho clients ----------

type pahoOptions struct {
	keepalive time.Duration
	onLost    func(paho.Client, error)
	handler   paho.MessageHandler
}

func newPahoClient(addr string, dev config.Device, extra pahoOptions) paho.Client {
	keepalive := extra.keepalive
	if keepalive == 0 {
		keepalive = 10 * time.Second
	}
	o := paho.NewClientOptions().
		AddBroker("tcp://" + addr).
		SetClientID(dev.ClientID).
		SetUsername(dev.Username).
		SetPassword(dev.Password).
		SetProtocolVersion(4).
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetKeepAlive(keepalive).
		SetPingTimeout(1 * time.Second).
		SetConnectTimeout(3 * time.Second).
		SetWriteTimeout(2 * time.Second).
		SetOrderMatters(false)
	if extra.onLost != nil {
		o.SetConnectionLostHandler(extra.onLost)
	}
	if extra.handler != nil {
		o.SetDefaultPublishHandler(extra.handler)
	}
	return paho.NewClient(o)
}

func connectPaho(t *testing.T, client paho.Client) {
	t.Helper()
	tok := client.Connect()
	if !tok.WaitTimeout(3 * time.Second) {
		t.Fatalf("paho CONNECT timed out")
	}
	if err := tok.Error(); err != nil {
		t.Fatalf("paho CONNECT failed: %v", err)
	}
}

// connectDevice returns a connected paho client whose default publish handler
// forwards messages to the returned channel.
func connectDevice(t *testing.T, addr string, dev config.Device) (paho.Client, chan paho.Message) {
	t.Helper()
	incoming := make(chan paho.Message, 16)
	client := newPahoClient(addr, dev, pahoOptions{
		handler: func(_ paho.Client, m paho.Message) {
			select {
			case incoming <- m:
			default:
			}
		},
	})
	connectPaho(t, client)
	t.Cleanup(func() { client.Disconnect(100) })
	return client, incoming
}

func subscribe(t *testing.T, client paho.Client, filter string, qos byte) map[string]byte {
	t.Helper()
	tok := client.Subscribe(filter, qos, nil)
	if !tok.WaitTimeout(3 * time.Second) {
		t.Fatalf("SUBSCRIBE %q timed out", filter)
	}
	if err := tok.Error(); err != nil {
		t.Fatalf("SUBSCRIBE %q failed: %v", filter, err)
	}
	st, ok := tok.(*paho.SubscribeToken)
	if !ok {
		t.Fatalf("unexpected subscribe token type %T", tok)
	}
	return st.Result()
}

func publish(t *testing.T, client paho.Client, topic string, payload []byte, qos byte, retain bool) paho.Token {
	t.Helper()
	tok := client.Publish(topic, qos, retain, payload)
	if !tok.WaitTimeout(3 * time.Second) {
		t.Fatalf("PUBLISH %q qos%d timed out", topic, qos)
	}
	return tok
}

func waitPahoMessage(t *testing.T, ch <-chan paho.Message, timeout time.Duration) paho.Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for downlink MQTT message")
		return nil
	}
}

func assertNoPahoMessage(t *testing.T, ch <-chan paho.Message, duration time.Duration, what string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("%s: unexpected MQTT message on %q: %q", what, m.Topic(), string(m.Payload()))
	case <-time.After(duration):
	}
}

// ---------- raw MQTT bytes ----------

func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("raw dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeAll(t *testing.T, conn net.Conn, data []byte) error {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// readRawPacket reads one MQTT packet and returns its type byte and remaining
// payload bytes.
func readRawPacket(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		t.Fatalf("read mqtt packet type: %v", err)
	}
	remaining, err := readVarint(conn)
	if err != nil {
		t.Fatalf("read mqtt remaining length: %v", err)
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("read mqtt packet body: %v", err)
	}
	return first[0], body
}

func readVarint(conn net.Conn) (int, error) {
	value, multiplier := 0, 1
	for i := 0; i < 4; i++ {
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return 0, err
		}
		value += int(b[0]&127) * multiplier
		multiplier *= 128
		if b[0]&128 == 0 {
			return value, nil
		}
	}
	return 0, fmt.Errorf("malformed remaining length")
}

func appendVarint(dst []byte, n int) []byte {
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		dst = append(dst, digit)
		if n == 0 {
			return dst
		}
	}
}

func appendMQTTString(dst []byte, s string) []byte {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	dst = append(dst, l[:]...)
	return append(dst, s...)
}

// connectPacket builds a synthetic MQTT CONNECT packet. usernames/passwords
// that are empty are omitted entirely (with matching flag bits), so callers
// can express "no credentials" as well as wrong credentials.
func connectPacket(clientID, username, password string, protocolLevel byte, keepalive uint16) []byte {
	protocolName := "MQTT"
	var flags byte = 0x02 // clean session
	if protocolLevel == 3 {
		protocolName = "MQIsdp"
	}
	if username != "" {
		flags |= 0x80
	}
	if password != "" {
		flags |= 0x40
	}

	var variable []byte
	variable = appendMQTTString(variable, protocolName)
	variable = append(variable, protocolLevel, flags)
	variable = append(variable, byte(keepalive>>8), byte(keepalive))
	if protocolLevel >= 5 {
		variable = append(variable, 0x00) // zero-length connect properties
	}

	var payload []byte
	payload = appendMQTTString(payload, clientID)
	if username != "" {
		payload = appendMQTTString(payload, username)
	}
	if password != "" {
		payload = appendMQTTString(payload, password)
	}

	packet := []byte{0x10}
	packet = appendVarint(packet, len(variable)+len(payload))
	packet = append(packet, variable...)
	packet = append(packet, payload...)
	return packet
}

func publishPacket(topic string, payload []byte, qos byte, retain bool, packetID uint16) []byte {
	header := byte(0x30) | qos<<1
	if retain {
		header |= 0x01
	}
	var body []byte
	body = appendMQTTString(body, topic)
	if qos > 0 {
		body = append(body, byte(packetID>>8), byte(packetID))
	}
	body = append(body, payload...)
	packet := []byte{header}
	packet = appendVarint(packet, len(body))
	return append(packet, body...)
}

func subscribePacket(packetID uint16, filter string, qos byte) []byte {
	var body []byte
	body = append(body, byte(packetID>>8), byte(packetID))
	body = appendMQTTString(body, filter)
	body = append(body, qos)
	packet := []byte{0x82}
	packet = appendVarint(packet, len(body))
	return append(packet, body...)
}

// rawConnect dials, sends CONNECT and returns the connection plus the CONNACK
// return code. Only the accept (0x00) and MQTT 3.1.1 error CONNACKs are
// expected by these helpers.
func rawConnect(t *testing.T, addr string, dev config.Device, protocolLevel byte, keepalive uint16) (net.Conn, byte) {
	t.Helper()
	conn := dialRaw(t, addr)
	if err := writeAll(t, conn, connectPacket(dev.ClientID, dev.Username, dev.Password, protocolLevel, keepalive)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	packetType, body := readRawPacket(t, conn)
	if packetType != 0x20 {
		t.Fatalf("expected CONNACK (0x20), got 0x%02x", packetType)
	}
	if len(body) < 2 {
		t.Fatalf("short CONNACK: %v", body)
	}
	return conn, body[1]
}

// silentRead waits until the peer closes the connection (or the deadline
// expires) and returns how long that took. It fails the test if the
// connection is still open after timeout.
func silentRead(t *testing.T, conn net.Conn, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 256)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("connection still open after %s (expected the broker to close it)", timeout)
			}
			return time.Since(start)
		}
	}
}

// ---------- misc ----------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitOffline(t *testing.T, b *Broker, deviceID string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "device "+deviceID+" offline", func() bool {
		_, ok := b.Online(deviceID)
		return !ok
	})
}

// lockedBuffer is a concurrency-safe log sink for redaction assertions.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogger(secrets []string) (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return logging.New(buf, slog.LevelDebug, logging.NewRedactor(secrets...)), buf
}

func assertLogsDoNotContain(t *testing.T, logs *lockedBuffer, needles ...string) {
	t.Helper()
	out := logs.String()
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		if strings.Contains(out, needle) {
			t.Fatalf("broker logs leaked a secret/payload marker %q:\n%s", needle, out)
		}
	}
}
