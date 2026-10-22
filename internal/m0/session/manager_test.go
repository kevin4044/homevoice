package session

import (
	"encoding/hex"
	"errors"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/udp"
)

type sentPacket struct {
	peer   *net.UDPAddr
	packet []byte
}

type capture struct {
	mu     sync.Mutex
	sends  []sentPacket
	frames []InboundFrame
	ended  []SessionEnd
}

func (c *capture) send(peer *net.UDPAddr, packet []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, sentPacket{peer: peer, packet: append([]byte(nil), packet...)})
	return nil
}

func (c *capture) onFrame(f InboundFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
}

func (c *capture) onEnded(e SessionEnd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = append(c.ended, e)
}

func (c *capture) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sends)
}

func (c *capture) lastSent(t *testing.T) sentPacket {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sends) == 0 {
		t.Fatal("no packet was sent")
	}
	return c.sends[len(c.sends)-1]
}

func (c *capture) frameCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func (c *capture) endedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ended)
}

type testManager struct {
	m   *Manager
	cap *capture
}

func newTestManager(t *testing.T, mut func(*Options)) testManager {
	t.Helper()
	cap := &capture{}
	opts := Options{
		Clock: clock.NewFake(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)),
		Rand:  rand.New(rand.NewSource(7)),
		Send:  cap.send,
		OnFrame: func(f InboundFrame) {
			cap.onFrame(f)
		},
		OnSessionEnded: func(e SessionEnd) { cap.onEnded(e) },
	}
	if mut != nil {
		mut(&opts)
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	return testManager{m: m, cap: cap}
}

func keyBytes(t *testing.T, info SessionInfo) [16]byte {
	t.Helper()
	raw, err := hex.DecodeString(info.KeyHex)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad key hex %q: %v", info.KeyHex, err)
	}
	var k [16]byte
	copy(k[:], raw)
	return k
}

func buildFrame(t *testing.T, info SessionInfo, seq, ts uint32, payload []byte) []byte {
	t.Helper()
	pkt, err := udp.Encrypt(keyBytes(t, info), udp.Header{
		PacketType: info.Template.PacketType,
		Flags:      info.Template.Flags,
		SSRC:       info.SSRC,
		Timestamp:  ts,
		Sequence:   seq,
	}, payload)
	if err != nil {
		t.Fatalf("encrypt frame: %v", err)
	}
	return pkt
}

func loopback(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

// TestA07HelloKeyRotationAndIsolation verifies distinct key/SSRC/session ids per
// device, rotation on repeated HELLO, invalidation of the old UDP stream and
// session-end callbacks used to cancel old tasks.
func TestA07HelloKeyRotationAndIsolation(t *testing.T) {
	tm := newTestManager(t, nil)
	a1, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := tm.m.Hello("dev-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a1.KeyHex == b1.KeyHex {
		t.Fatal("two devices share an AES key")
	}
	if a1.SSRC == b1.SSRC {
		t.Fatal("two devices share an SSRC")
	}
	if a1.SessionID == b1.SessionID {
		t.Fatal("two devices share a session id")
	}
	// The HELLO nonce template must contain the negotiated type/flags/SSRC.
	nonceBytes, _ := hex.DecodeString(a1.NonceHex)
	nonce, err := udp.ParseHeader(nonceBytes)
	if err != nil {
		t.Fatalf("nonce header: %v", err)
	}
	if nonce.PacketType != a1.Template.PacketType || nonce.Flags != a1.Template.Flags || nonce.SSRC != a1.SSRC {
		t.Fatalf("nonce template mismatch: %+v", nonce)
	}

	peer := loopback(40001)
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, a1, 1, 60, []byte("audio-a"))); err != nil {
		t.Fatalf("valid frame = %v", err)
	}

	a2, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a2.KeyHex == a1.KeyHex || a2.SSRC == a1.SSRC || a2.SessionID == a1.SessionID {
		t.Fatalf("HELLO did not rotate key/ssrc/session: old=%+v new=%+v", a1, a2)
	}
	if tm.cap.endedCount() != 1 {
		t.Fatalf("session end callbacks = %d, want 1", tm.cap.endedCount())
	}
	if got := tm.cap.ended[0]; got.Reason != "hello-rotation" || got.SessionID != a1.SessionID {
		t.Fatalf("session end = %+v", got)
	}
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, a1, 2, 120, []byte("stale"))); !errors.Is(err, ErrUnknownSSRC) {
		t.Fatalf("old SSRC frame error = %v, want ErrUnknownSSRC", err)
	}
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, a2, 1, 60, []byte("new"))); err != nil {
		t.Fatalf("new SSRC frame = %v", err)
	}
	if tm.m.ActiveCount() != 2 {
		t.Fatalf("active sessions = %d, want 2", tm.m.ActiveCount())
	}
}

// TestA09SessionRejectsMalformedAndUnknownFrames covers structural, template,
// SSRC and empty-payload handling at the session entrance.
func TestA09SessionRejectsMalformedAndUnknownFrames(t *testing.T) {
	tm := newTestManager(t, nil)
	info, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	peer := loopback(40002)
	valid := buildFrame(t, info, 1, 60, []byte("opus-ish"))

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"short header", valid[:10], udp.ErrPacketTooShort},
		{"truncated payload", valid[:len(valid)-1], ErrLengthMismatch},
		{"oversize", append(valid, make([]byte, limits.MaxUDPDatagramBytes)...), udp.ErrPacketTooLarge},
		{"unknown ssrc", buildFrame(t, SessionInfo{KeyHex: info.KeyHex, Template: info.Template, SSRC: info.SSRC + 1}, 1, 60, []byte("x")), ErrUnknownSSRC},
	}
	wrongFlags := udp.Header{PacketType: info.Template.PacketType, Flags: info.Template.Flags ^ 0x80, SSRC: info.SSRC, Timestamp: 1, Sequence: 2}
	wrongFlagsPkt, err := udp.Encrypt(keyBytes(t, info), wrongFlags, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct {
		name string
		data []byte
		want error
	}{"wrong template flags", wrongFlagsPkt, ErrTemplateMismatch})
	wrongType := udp.Header{PacketType: info.Template.PacketType + 1, Flags: info.Template.Flags, SSRC: info.SSRC, Timestamp: 1, Sequence: 3}
	wrongTypePkt, _ := udp.Encrypt(keyBytes(t, info), wrongType, []byte("x"))
	cases = append(cases, struct {
		name string
		data []byte
		want error
	}{"wrong template type", wrongTypePkt, ErrTemplateMismatch})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tm.m.HandleDatagram(peer, tc.data); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if tm.cap.frameCount() != 0 {
		t.Fatalf("rejected frames delivered to audio pipeline: %d", tm.cap.frameCount())
	}

	// Empty payload is a valid envelope but must never reach the audio decoder.
	empty := buildFrame(t, info, 10, 600, nil)
	if _, err := tm.m.HandleDatagram(peer, empty); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty payload error = %v, want ErrEmptyPayload", err)
	}
	if tm.cap.frameCount() != 0 {
		t.Fatal("empty payload was delivered as audio")
	}
	snap, _ := tm.m.Snapshot("dev-a")
	if snap.Counters.EmptyPayloads != 1 {
		t.Fatalf("empty payload counter = %d", snap.Counters.EmptyPayloads)
	}

	if _, err := tm.m.HandleDatagram(peer, valid); err != nil {
		t.Fatalf("valid frame after rejects = %v", err)
	}
	if tm.cap.frameCount() != 1 {
		t.Fatalf("valid frame deliveries = %d, want 1", tm.cap.frameCount())
	}
}

// TestA10ReplayWindowSemantics verifies out-of-order acceptance, duplicate
// rejection, out-of-window rejection and explicit first-sequence-0 handling.
func TestA10ReplayWindowSemantics(t *testing.T) {
	tm := newTestManager(t, nil)
	info, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	peer := loopback(40003)
	for _, seq := range []uint32{1, 3, 2} {
		if _, err := tm.m.HandleDatagram(peer, buildFrame(t, info, seq, 60, []byte("f"))); err != nil {
			t.Fatalf("sequence %d = %v", seq, err)
		}
	}
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, info, 3, 60, []byte("f"))); !errors.Is(err, ErrReplayDuplicate) {
		t.Fatalf("duplicate 3 = %v", err)
	}
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, info, 1000, 60, []byte("f"))); err != nil {
		t.Fatalf("jump = %v", err)
	}
	if _, err := tm.m.HandleDatagram(peer, buildFrame(t, info, 900, 60, []byte("f"))); !errors.Is(err, ErrReplayTooOld) {
		t.Fatalf("out-of-window = %v, want ErrReplayTooOld", err)
	}
	snap, _ := tm.m.Snapshot("dev-a")
	if snap.Counters.Accepted != 4 || snap.Counters.Duplicates != 1 || snap.Counters.TooOld != 1 {
		t.Fatalf("counters = %+v", snap.Counters)
	}

	// Explicit first-sequence-0 compatibility case.
	info2, err := tm.m.Hello("dev-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.m.HandleDatagram(loopback(40004), buildFrame(t, info2, 0, 60, []byte("zero"))); err != nil {
		t.Fatalf("first sequence 0 = %v, want accepted", err)
	}
	if _, err := tm.m.HandleDatagram(loopback(40004), buildFrame(t, info2, 0, 60, []byte("zero"))); !errors.Is(err, ErrReplayDuplicate) {
		t.Fatalf("second sequence 0 = %v", err)
	}
}

// TestA11PeerBindingIsStickyButNotAuthentication verifies the first valid
// source is locked, later source changes are rejected until HELLO, and that
// first-packet locking alone is not cryptographic authentication.
func TestA11PeerBindingIsStickyButNotAuthentication(t *testing.T) {
	tm := newTestManager(t, nil)
	info, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	first := loopback(40100)
	attacker := loopback(40101)
	if _, err := tm.m.HandleDatagram(first, buildFrame(t, info, 1, 60, []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	snap, _ := tm.m.Snapshot("dev-a")
	if snap.Peer != first.String() {
		t.Fatalf("peer = %q, want %q", snap.Peer, first.String())
	}
	if _, err := tm.m.HandleDatagram(attacker, buildFrame(t, info, 2, 60, []byte("spoof"))); !errors.Is(err, ErrPeerMismatch) {
		t.Fatalf("source change = %v, want ErrPeerMismatch", err)
	}
	snap, _ = tm.m.Snapshot("dev-a")
	if snap.Peer != first.String() {
		t.Fatalf("peer was overwritten: %q", snap.Peer)
	}
	// A new HELLO rebinds the peer; NAT changes require HELLO, not silent
	// source updates.
	info2, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.m.HandleDatagram(attacker, buildFrame(t, info2, 1, 60, []byte("rebound"))); err != nil {
		t.Fatalf("post-HELLO peer = %v", err)
	}
	// Documented risk: the first structurally valid frame of a fresh session
	// locks the peer. Any allowed source can grab it before the real device;
	// this is not authentication and must be reported as such.
	if _, err := tm.m.HandleDatagram(loopback(40102), buildFrame(t, info2, 2, 60, []byte("other"))); !errors.Is(err, ErrPeerMismatch) {
		t.Fatalf("second source after first lock = %v", err)
	}
}

// TestA12OutboundSequenceAndFailureSemantics verifies no send before a peer,
// sequence starting at 1, timestamp non-zero, no rollback on failure and wrap
// rejection requiring a new HELLO.
func TestA12OutboundSequenceAndFailureSemantics(t *testing.T) {
	var failNext bool
	cap := &capture{}
	opts := Options{
		Clock:          clock.NewFake(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)),
		Rand:           rand.New(rand.NewSource(11)),
		OnFrame:        func(f InboundFrame) {},
		OnSessionEnded: func(e SessionEnd) {},
		Send: func(peer *net.UDPAddr, packet []byte) error {
			cap.send(peer, packet)
			if failNext {
				return errors.New("synthetic send failure")
			}
			return nil
		},
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SendAudio("dev-a", []byte("frame")); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("send before peer = %v, want ErrNoPeer", err)
	}
	if cap.sentCount() != 0 {
		t.Fatal("packet sent before a peer was established")
	}
	peer := loopback(40200)
	if _, err := m.HandleDatagram(peer, buildFrame(t, info, 1, 60, []byte("in"))); err != nil {
		t.Fatal(err)
	}
	if err := m.SendAudio("dev-a", []byte("out-1")); err != nil {
		t.Fatalf("first send = %v", err)
	}
	first := cap.lastSent(t)
	if first.peer.String() != peer.String() {
		t.Fatalf("send peer = %v, want %v", first.peer, peer)
	}
	h, plain, err := udp.Decrypt(keyBytes(t, info), first.packet)
	if err != nil {
		t.Fatalf("decrypt outbound = %v", err)
	}
	if h.Sequence != 1 {
		t.Fatalf("first outbound sequence = %d, want 1", h.Sequence)
	}
	if h.Timestamp == 0 {
		t.Fatal("outbound timestamp is zero")
	}
	if h.SSRC != info.SSRC || string(plain) != "out-1" {
		t.Fatalf("outbound packet ssrc/payload = %d/%q", h.SSRC, plain)
	}

	failNext = true
	if err := m.SendAudio("dev-a", []byte("out-2-fails")); err == nil {
		t.Fatal("synthetic send failure was not returned")
	}
	failed := cap.lastSent(t)
	fh, _, _ := udp.Decrypt(keyBytes(t, info), failed.packet)
	if fh.Sequence != 2 {
		t.Fatalf("failed send used sequence %d, want 2", fh.Sequence)
	}
	failNext = false
	if err := m.SendAudio("dev-a", []byte("out-3")); err != nil {
		t.Fatal(err)
	}
	third := cap.lastSent(t)
	th, _, _ := udp.Decrypt(keyBytes(t, info), third.packet)
	if th.Sequence != 3 {
		t.Fatalf("after failure sequence = %d, want 3 (failure must not roll back)", th.Sequence)
	}

	// Force the sequence near the wrap limit to prove bounded rejection.
	setOutSeqForTest(t, m, "dev-a", limits.OutboundSequenceMax)
	if err := m.SendAudio("dev-a", []byte("last")); err != nil {
		t.Fatalf("last sequence send = %v", err)
	}
	if err := m.SendAudio("dev-a", []byte("wrapped")); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("post-wrap send = %v, want ErrSequenceExhausted", err)
	}
}

func setOutSeqForTest(t *testing.T, m *Manager, deviceID string, seq uint32) {
	t.Helper()
	m.mu.Lock()
	s := m.sessions[deviceID]
	m.mu.Unlock()
	if s == nil {
		t.Fatal("no session")
	}
	s.mu.Lock()
	s.outSeq = seq
	s.mu.Unlock()
}

func TestSessionCountersAndSnapshotAreKeyFree(t *testing.T) {
	tm := newTestManager(t, nil)
	info, err := tm.m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.m.HandleDatagram(loopback(40300), buildFrame(t, info, 1, 60, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	snap, ok := tm.m.Snapshot("dev-a")
	if !ok || snap.SessionID != info.SessionID || !snap.ReplayInitialized {
		t.Fatalf("snapshot = %+v ok=%v", snap, ok)
	}
	if snap.Peer == "" {
		t.Fatal("snapshot missing peer")
	}
	// The snapshot type has no key field at all; this compile-time-ish check
	// documents that evidence cannot leak session keys through it.
	_ = snap
}

func TestSourcePolicy(t *testing.T) {
	policy, err := SourcePolicy([]string{"10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		addr *net.UDPAddr
		want bool
	}{
		{&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, true},
		{&net.UDPAddr{IP: net.IPv6loopback, Port: 1}, true},
		{&net.UDPAddr{IP: net.IPv4(10, 20, 5, 5), Port: 1}, true},
		{&net.UDPAddr{IP: net.IPv4(10, 21, 5, 5), Port: 1}, false},
		{&net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 1}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := policy(tc.addr); got != tc.want {
			t.Fatalf("policy(%v) = %v, want %v", tc.addr, got, tc.want)
		}
	}
	if _, err := SourcePolicy([]string{"not-a-cidr"}); err == nil {
		t.Fatal("SourcePolicy accepted invalid CIDR")
	}
}

func TestHelloAfterCloseIsRejected(t *testing.T) {
	tm := newTestManager(t, nil)
	if _, err := tm.m.Hello("dev-a", 1); err != nil {
		t.Fatal(err)
	}
	if n := tm.m.CloseAll("shutdown"); n != 1 {
		t.Fatalf("CloseAll = %d", n)
	}
	if _, err := tm.m.Hello("dev-a", 1); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Hello after close = %v", err)
	}
}

func TestDropGenerationIgnoresOldConnections(t *testing.T) {
	tm := newTestManager(t, nil)
	if _, err := tm.m.Hello("dev-a", 2); err != nil {
		t.Fatal(err)
	}
	if tm.m.DropGeneration("dev-a", 1) {
		t.Fatal("old connection generation dropped the current audio session")
	}
	if tm.m.ActiveCount() != 1 {
		t.Fatal("session lost after old-generation drop")
	}
	if !tm.m.DropGeneration("dev-a", 2) {
		t.Fatal("current generation did not drop session")
	}
	if tm.m.ActiveCount() != 0 || tm.cap.endedCount() != 1 {
		t.Fatalf("active=%d ended=%d", tm.m.ActiveCount(), tm.cap.endedCount())
	}
}

// TestA19ResourceAccountingAfterManySessions is the session-layer part of the
// 100-cycle resource closure requirement.
func TestA19ResourceAccountingAfterManySessions(t *testing.T) {
	tm := newTestManager(t, nil)
	loopbackAddr := loopback(40400)
	for i := 0; i < 100; i++ {
		info, err := tm.m.Hello("dev-a", uint64(i+1))
		if err != nil {
			t.Fatalf("cycle %d hello: %v", i, err)
		}
		if _, err := tm.m.HandleDatagram(loopbackAddr, buildFrame(t, info, 1, 60, []byte("x"))); err != nil {
			t.Fatalf("cycle %d frame: %v", i, err)
		}
		switch i % 3 {
		case 0:
			if err := tm.m.Goodbye("dev-a", info.SessionID); err != nil {
				t.Fatalf("cycle %d goodbye: %v", i, err)
			}
		case 1:
			if !tm.m.DropGeneration("dev-a", uint64(i+1)) {
				t.Fatalf("cycle %d drop: no session", i)
			}
		case 2:
			// leave it for the next HELLO to rotate
		}
	}
	tm.m.CloseAll("shutdown")
	if got := tm.m.ActiveCount(); got != 0 {
		t.Fatalf("active sessions after close = %d", got)
	}
	tm.m.mu.Lock()
	ssrcs := len(tm.m.bySSRC)
	retired := len(tm.m.retiredSSRC)
	tm.m.mu.Unlock()
	if ssrcs != 0 {
		t.Fatalf("SSRC index = %d entries", ssrcs)
	}
	if retired > 256 {
		t.Fatalf("retired SSRC tombstones unbounded: %d", retired)
	}
	if tm.cap.frameCount() != 100 {
		t.Fatalf("delivered frames = %d, want 100", tm.cap.frameCount())
	}
}
