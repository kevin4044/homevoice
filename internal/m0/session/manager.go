// Package session manages the MQTT-independent UDP audio sessions for the M0
// probe. A session owns a random AES-128 key, SSRC, protocol session id,
// packet template, source binding, replay window and outbound sequence state.
//
// Security boundary: a valid session packet is NOT authenticated. AES-CTR has
// no integrity tag; peer locking and the replay window only limit accidental or
// unsolicited reuse and are explicitly not claimed as authentication.
package session

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/udp"
)

// Session and datagram errors. They describe routing/framing, not
// authentication.
var (
	ErrNoSession         = errors.New("no active audio session for device")
	ErrNoPeer            = errors.New("no UDP peer established for session")
	ErrUnknownSSRC       = errors.New("unknown or retired UDP SSRC")
	ErrTemplateMismatch  = errors.New("udp packet type/flags do not match negotiated template")
	ErrLengthMismatch    = errors.New("udp declared payload length does not match datagram")
	ErrSourceRejected    = errors.New("udp source address not allowed by configuration")
	ErrPeerMismatch      = errors.New("udp source address differs from the bound session peer")
	ErrReplayDuplicate   = errors.New("udp sequence already accepted")
	ErrReplayTooOld      = errors.New("udp sequence outside replay window")
	ErrSequenceExhausted = errors.New("outbound udp sequence exhausted; re-establish via HELLO")
	ErrEmptyPayload      = errors.New("udp audio payload is empty")
	ErrPayloadTooLarge   = errors.New("udp audio payload exceeds probe limit")
	ErrSessionMismatch   = errors.New("session_id does not match the current audio session")
	ErrManagerClosed     = errors.New("session manager closed")
)

// Counters are per-session cumulative counters safe to include in reports.
type Counters struct {
	Accepted        uint64
	Duplicates      uint64
	TooOld          uint64
	UnknownSSRC     uint64 // manager-level: no single session to attribute
	TemplateRejects uint64
	SourceRejects   uint64
	PeerRejects     uint64
	EmptyPayloads   uint64
	Sent            uint64
	SendErrors      uint64
}

// InboundFrame is one structurally valid, in-window, non-empty audio frame.
type InboundFrame struct {
	DeviceID       string
	SessionID      string
	ConnGeneration uint64
	Header         udp.Header
	Payload        []byte // decrypted Opus bytes; never a fragment of plaintext header
	ReceivedAt     time.Time
}

// SessionEnd describes why an audio session was released and is used by the
// app to cancel pending playback/requests and drop old tasks.
type SessionEnd struct {
	DeviceID       string
	SessionID      string
	ConnGeneration uint64
	Reason         string
}

// Options configures a Manager. Callbacks run without manager locks and must
// not block.
type Options struct {
	Clock          clock.Clock
	Rand           io.Reader // defaults to crypto/rand.Reader in production
	Logger         *slog.Logger
	Send           func(peer *net.UDPAddr, datagram []byte) error
	AllowedSource  func(addr *net.UDPAddr) bool
	FrameDuration  time.Duration
	OnFrame        func(InboundFrame)
	OnSessionEnded func(SessionEnd)
}

// Manager owns the current audio session per device.
type Manager struct {
	mu          sync.Mutex
	opts        Options
	sessions    map[string]*Session
	bySSRC      map[uint32]*Session
	retiredSSRC map[uint32]struct{}
	retiredFIFO []uint32
	closed      bool

	unknownSSRC  uint64
	malformed    uint64
	sourceReject uint64
}

// Session is the internal session state. Access it through Manager methods.
type Session struct {
	mu sync.Mutex

	deviceID  string
	id        string
	connGen   uint64
	key       [16]byte
	ssrc      uint32
	template  udp.Template
	nonce     [16]byte
	createdAt time.Time

	peer         *net.UDPAddr
	replay       *ReplayWindow
	closed       bool
	outSeq       uint32
	outTimestamp uint32
	counters     Counters
}

// SessionInfo is the safe handoff of HELLO material to the MQTT layer.
type SessionInfo struct {
	DeviceID       string
	SessionID      string
	ConnGeneration uint64
	KeyHex         string
	NonceHex       string
	SSRC           uint32
	Template       udp.Template
	CreatedAt      time.Time
}

// Snapshot is a point-in-time copy for tests and reports. It deliberately
// excludes key material so it can be logged or written to evidence.
type Snapshot struct {
	DeviceID          string
	SessionID         string
	ConnGeneration    uint64
	SSRC              uint32
	Template          udp.Template
	Peer              string
	CreatedAt         time.Time
	Closed            bool
	NextOutboundSeq   uint32
	OutboundTimestamp uint32
	Counters          Counters
	ReplayHighest     uint32
	ReplayInitialized bool
}

// NewManager creates a session manager. Send and Clock are required; Rand
// defaults to crypto/rand.Reader and must never be replaced by UUID/MAC-based
// key material in production.
func NewManager(opts Options) (*Manager, error) {
	if opts.Send == nil {
		return nil, errors.New("session manager requires a Send function")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Logger == nil {
		opts.Logger = logging.Discard()
	}
	if opts.FrameDuration <= 0 {
		opts.FrameDuration = limits.DownlinkFrameDurationMS * time.Millisecond
	}
	if opts.AllowedSource == nil {
		opts.AllowedSource = DefaultSourceAllowed
	}
	return &Manager{
		opts:        opts,
		sessions:    make(map[string]*Session),
		bySSRC:      make(map[uint32]*Session),
		retiredSSRC: make(map[uint32]struct{}),
	}, nil
}

// DefaultSourceAllowed accepts loopback only. A private/link-local source must
// be explicitly authorized through SourcePolicy.
func DefaultSourceAllowed(addr *net.UDPAddr) bool {
	if addr == nil {
		return false
	}
	return addr.IP != nil && addr.IP.IsLoopback()
}

// SourcePolicy builds a source-address predicate: loopback is always allowed;
// non-loopback addresses must fall in one of the explicitly authorized CIDRs.
func SourcePolicy(authorizedCIDRs []string) (func(*net.UDPAddr) bool, error) {
	prefixes := make([]netip.Prefix, 0, len(authorizedCIDRs))
	for _, c := range authorizedCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("invalid authorized source CIDR %q: %w", c, err)
		}
		prefixes = append(prefixes, p.Masked())
	}
	return func(addr *net.UDPAddr) bool {
		if addr == nil || addr.IP == nil {
			return false
		}
		if addr.IP.IsLoopback() {
			return true
		}
		ip, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			return false
		}
		ip = ip.Unmap()
		for _, p := range prefixes {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}, nil
}

// Hello creates or rotates the audio session for an authenticated MQTT
// device/generation. Every HELLO replaces the key, SSRC, session id, template,
// peer and sequence state, and schedules end of the previous session.
func (m *Manager) Hello(deviceID string, connGen uint64) (SessionInfo, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return SessionInfo{}, ErrManagerClosed
	}
	key, err := m.randomN(16)
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	sidRaw, err := m.randomN(16)
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	ssrc, err := m.newSSRC()
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	var keyArr [16]byte
	copy(keyArr[:], key)
	tpl := udp.Template{PacketType: 0x01, Flags: 0x00}
	nonce := udp.Header{
		PacketType: tpl.PacketType,
		Flags:      tpl.Flags,
		PayloadLen: 0,
		SSRC:       ssrc,
	}.Marshal()
	s := &Session{
		deviceID:  deviceID,
		id:        hex.EncodeToString(sidRaw),
		connGen:   connGen,
		key:       keyArr,
		ssrc:      ssrc,
		template:  tpl,
		nonce:     nonce,
		createdAt: m.opts.Clock.Now(),
		replay:    NewReplayWindow(limits.UDPReplayWindow),
		outSeq:    1,
	}
	var ended []SessionEnd
	if old, ok := m.sessions[deviceID]; ok {
		ended = append(ended, m.removeLocked(old, "hello-rotation"))
	}
	m.sessions[deviceID] = s
	m.bySSRC[ssrc] = s
	m.mu.Unlock()

	for _, e := range ended {
		m.opts.OnSessionEnded(e)
	}
	return SessionInfo{
		DeviceID:       deviceID,
		SessionID:      s.id,
		ConnGeneration: connGen,
		KeyHex:         hex.EncodeToString(keyArr[:]),
		NonceHex:       hex.EncodeToString(nonce[:]),
		SSRC:           ssrc,
		Template:       tpl,
		CreatedAt:      s.createdAt,
	}, nil
}

// Goodbye releases the current audio session while leaving MQTT untouched. An
// empty sessionID means the current session; a stale non-matching id is
// ignored with ErrSessionMismatch.
func (m *Manager) Goodbye(deviceID, sessionID string) error {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	if !ok {
		m.mu.Unlock()
		return ErrNoSession
	}
	if sessionID != "" && sessionID != s.id {
		m.mu.Unlock()
		return ErrSessionMismatch
	}
	e := m.removeLocked(s, "goodbye")
	m.mu.Unlock()
	m.opts.OnSessionEnded(e)
	return nil
}

// DropGeneration removes the audio session only if it belongs to the given MQTT
// connection generation. A late disconnect callback for an old connection must
// not release a newer session.
func (m *Manager) DropGeneration(deviceID string, connGen uint64) bool {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	if !ok || s.connGen != connGen {
		m.mu.Unlock()
		return false
	}
	e := m.removeLocked(s, "mqtt-disconnect")
	m.mu.Unlock()
	m.opts.OnSessionEnded(e)
	return true
}

// CloseAll releases every session (probe shutdown / MQTT-wide cleanup) and
// prevents new sessions.
func (m *Manager) CloseAll(reason string) int {
	m.mu.Lock()
	m.closed = true
	ended := make([]SessionEnd, 0, len(m.sessions))
	for _, s := range m.sessions {
		ended = append(ended, m.removeLocked(s, reason))
	}
	m.mu.Unlock()
	for _, e := range ended {
		m.opts.OnSessionEnded(e)
	}
	return len(ended)
}

// ActiveCount returns the number of live sessions (test/resource accounting).
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// removeLocked unlinks the session and retires its SSRC. Caller holds m.mu.
func (m *Manager) removeLocked(s *Session, reason string) SessionEnd {
	if s == nil {
		return SessionEnd{}
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	delete(m.sessions, s.deviceID)
	delete(m.bySSRC, s.ssrc)
	m.retireSSRCLocked(s.ssrc)
	return SessionEnd{DeviceID: s.deviceID, SessionID: s.id, ConnGeneration: s.connGen, Reason: reason}
}

// retireSSRCLocked remembers a retired SSRC so a later random SSRC cannot
// resurrect a stale packet stream as the new session. The tombstone set is
// bounded; production keys remain random. Caller holds m.mu.
func (m *Manager) retireSSRCLocked(ssrc uint32) {
	if _, exists := m.retiredSSRC[ssrc]; exists {
		return
	}
	m.retiredSSRC[ssrc] = struct{}{}
	m.retiredFIFO = append(m.retiredFIFO, ssrc)
	const maxRetired = 256
	if len(m.retiredFIFO) > maxRetired {
		oldest := m.retiredFIFO[0]
		m.retiredFIFO = m.retiredFIFO[1:]
		delete(m.retiredSSRC, oldest)
	}
}

// HandleDatagram validates one inbound UDP datagram in the documented order:
// total length, declared length, type/flags + active SSRC, configured source,
// peer binding, then the 64-packet replay window. Empty payloads are accepted
// by the envelope but never delivered as audio.
func (m *Manager) HandleDatagram(from *net.UDPAddr, datagram []byte) (InboundFrame, error) {
	if len(datagram) < udp.HeaderSize {
		m.countMalformed()
		return InboundFrame{}, fmt.Errorf("udp: %w", udp.ErrPacketTooShort)
	}
	if len(datagram) > limits.MaxUDPDatagramBytes {
		m.countMalformed()
		return InboundFrame{}, fmt.Errorf("udp: %w", udp.ErrPacketTooLarge)
	}
	h, err := udp.ParseHeader(datagram)
	if err != nil {
		m.countMalformed()
		return InboundFrame{}, fmt.Errorf("udp: %w", err)
	}
	if int(h.PayloadLen) != len(datagram)-udp.HeaderSize {
		m.countMalformed()
		return InboundFrame{}, ErrLengthMismatch
	}

	m.mu.Lock()
	s, ok := m.bySSRC[h.SSRC]
	m.mu.Unlock()
	if !ok {
		m.mu.Lock()
		m.unknownSSRC++
		m.mu.Unlock()
		return InboundFrame{}, ErrUnknownSSRC
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InboundFrame{}, ErrNoSession
	}
	if !s.template.Matches(h) {
		s.counters.TemplateRejects++
		s.mu.Unlock()
		return InboundFrame{}, ErrTemplateMismatch
	}
	s.mu.Unlock()

	if !m.opts.AllowedSource(from) {
		m.mu.Lock()
		m.sourceReject++
		m.mu.Unlock()
		s.mu.Lock()
		s.counters.SourceRejects++
		s.mu.Unlock()
		return InboundFrame{}, ErrSourceRejected
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InboundFrame{}, ErrNoSession
	}
	if s.peer == nil {
		s.peer = cloneAddr(from)
	} else if !sameAddr(s.peer, from) {
		s.counters.PeerRejects++
		s.mu.Unlock()
		return InboundFrame{}, ErrPeerMismatch
	}
	switch s.replay.Accept(h.Sequence) {
	case ReplayDuplicate:
		s.counters.Duplicates++
		s.mu.Unlock()
		return InboundFrame{}, ErrReplayDuplicate
	case ReplayTooOld:
		s.counters.TooOld++
		s.mu.Unlock()
		return InboundFrame{}, ErrReplayTooOld
	}
	s.counters.Accepted++
	if h.PayloadLen == 0 {
		s.counters.EmptyPayloads++
		s.mu.Unlock()
		return InboundFrame{}, ErrEmptyPayload
	}
	key := s.key
	sid := s.id
	gen := s.connGen
	deviceID := s.deviceID
	s.mu.Unlock()

	header, payload, err := udp.Decrypt(key, datagram)
	if err != nil {
		return InboundFrame{}, err
	}
	frame := InboundFrame{
		DeviceID:       deviceID,
		SessionID:      sid,
		ConnGeneration: gen,
		Header:         header,
		Payload:        payload,
		ReceivedAt:     m.opts.Clock.Now(),
	}
	if m.opts.OnFrame != nil {
		m.opts.OnFrame(frame)
	}
	return frame, nil
}

// SendAudio encrypts and sends one downlink frame on the same socket used for
// inbound traffic. The sequence number is consumed before the send attempt: a
// failed send never reuses a sequence. The first sequence is 1; wrap requires a
// new HELLO.
func (m *Manager) SendAudio(deviceID string, frame []byte) error {
	if len(frame) == 0 {
		return ErrEmptyPayload
	}
	if len(frame) > limits.MaxUDPPayloadBytes {
		return ErrPayloadTooLarge
	}
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	m.mu.Unlock()
	if !ok {
		return ErrNoSession
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrNoSession
	}
	if s.peer == nil {
		s.mu.Unlock()
		return ErrNoPeer
	}
	if s.outSeq == 0 {
		s.mu.Unlock()
		return ErrSequenceExhausted
	}
	seq := s.outSeq
	ts := s.outTimestamp + uint32(m.opts.FrameDuration.Milliseconds())
	if ts == 0 {
		ts = 1
	}
	packet, err := udp.Encrypt(s.key, udp.Header{
		PacketType: s.template.PacketType,
		Flags:      s.template.Flags,
		SSRC:       s.ssrc,
		Timestamp:  ts,
		Sequence:   seq,
	}, frame)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.outTimestamp = ts
	if seq == limits.OutboundSequenceMax {
		s.outSeq = 0 // exhausted; next attempt is rejected until HELLO
	} else {
		s.outSeq = seq + 1
	}
	peer := cloneAddr(s.peer)
	s.mu.Unlock()

	sendErr := m.opts.Send(peer, packet)
	s.mu.Lock()
	if sendErr != nil {
		s.counters.SendErrors++
	} else {
		s.counters.Sent++
	}
	s.mu.Unlock()
	return sendErr
}

// Snapshot returns a safe point-in-time copy for tests/evidence.
func (m *Manager) Snapshot(deviceID string) (Snapshot, bool) {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	m.mu.Unlock()
	if !ok {
		return Snapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		DeviceID:          s.deviceID,
		SessionID:         s.id,
		ConnGeneration:    s.connGen,
		SSRC:              s.ssrc,
		Template:          s.template,
		CreatedAt:         s.createdAt,
		Closed:            s.closed,
		NextOutboundSeq:   s.outSeq,
		OutboundTimestamp: s.outTimestamp,
		Counters:          s.counters,
	}
	if s.peer != nil {
		snap.Peer = s.peer.String()
	}
	if s.replay != nil {
		_, _, _, highest, init := s.replay.Stats()
		snap.ReplayHighest = highest
		snap.ReplayInitialized = init
	}
	return snap, true
}

// ManagerCounters returns aggregate rejection counters for reports.
func (m *Manager) ManagerCounters() (unknownSSRC, malformed, sourceReject uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unknownSSRC, m.malformed, m.sourceReject
}

func (m *Manager) countMalformed() {
	m.mu.Lock()
	m.malformed++
	m.mu.Unlock()
}

func (m *Manager) randomN(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(m.opts.Rand, b); err != nil {
		return nil, fmt.Errorf("session random: %w", err)
	}
	return b, nil
}

func (m *Manager) newSSRC() (uint32, error) {
	for attempt := 0; attempt < 32; attempt++ {
		b, err := m.randomN(4)
		if err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint32(b)
		if v == 0 {
			continue
		}
		if _, active := m.bySSRC[v]; active {
			continue
		}
		if _, retired := m.retiredSSRC[v]; retired {
			continue
		}
		return v, nil
	}
	return 0, errors.New("session: failed to allocate a unique SSRC")
}

func cloneAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	ip := make(net.IP, len(a.IP))
	copy(ip, a.IP)
	return &net.UDPAddr{IP: ip, Port: a.Port, Zone: a.Zone}
}

func sameAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}
