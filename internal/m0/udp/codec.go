// Package udp implements the M0-v0.1 UDP audio envelope: a 16-byte cleartext
// header followed by an AES-128-CTR encrypted payload.
//
// SECURITY LIMITATION (must not be overstated): AES-CTR provides
// confidentiality only. There is NO authentication tag (no MAC/AEAD). A
// modified ciphertext decrypts to modified plaintext with no integrity error.
// Source-address locking, SSRC matching, template matching, the replay window
// and Opus decoding are NOT cryptographic authentication. Calling any of these
// "integrity protection" is prohibited by the M0 specification.
package udp

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// HeaderSize is the cleartext UDP header size in bytes.
const HeaderSize = limits.UDPHeaderBytes

// Operational errors returned by the codec. They describe framing only; none
// of them is an authentication failure.
var (
	ErrPacketTooShort  = errors.New("udp packet too short")
	ErrPacketTooLarge  = errors.New("udp packet exceeds 1500-byte probe limit")
	ErrLengthMismatch  = errors.New("udp declared payload length does not match datagram")
	ErrPayloadTooLarge = errors.New("udp payload exceeds probe limit")
)

// Header is the 16-byte cleartext UDP header. It is also the per-packet AES-CTR
// initial counter block.
type Header struct {
	PacketType byte
	Flags      byte
	PayloadLen uint16
	SSRC       uint32
	Timestamp  uint32
	Sequence   uint32
}

// Marshal returns the 16-byte wire representation.
func (h Header) Marshal() [HeaderSize]byte {
	var b [HeaderSize]byte
	b[0] = h.PacketType
	b[1] = h.Flags
	binary.BigEndian.PutUint16(b[2:4], h.PayloadLen)
	binary.BigEndian.PutUint32(b[4:8], h.SSRC)
	binary.BigEndian.PutUint32(b[8:12], h.Timestamp)
	binary.BigEndian.PutUint32(b[12:16], h.Sequence)
	return b
}

// ParseHeader reads the 16-byte header at the start of b.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: %d < %d", ErrPacketTooShort, len(b), HeaderSize)
	}
	return Header{
		PacketType: b[0],
		Flags:      b[1],
		PayloadLen: binary.BigEndian.Uint16(b[2:4]),
		SSRC:       binary.BigEndian.Uint32(b[4:8]),
		Timestamp:  binary.BigEndian.Uint32(b[8:12]),
		Sequence:   binary.BigEndian.Uint32(b[12:16]),
	}, nil
}

// Encrypt builds a full UDP datagram from a header template and plaintext.
// The header's PayloadLen is set to len(plaintext). The full 16-byte header is
// the AES counter initial value (never a fixed HELLO nonce).
func Encrypt(key [16]byte, h Header, plaintext []byte) ([]byte, error) {
	if len(plaintext) > limits.MaxUDPPayloadBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(plaintext), limits.MaxUDPPayloadBytes)
	}
	h.PayloadLen = uint16(len(plaintext))
	headerBytes := h.Marshal()
	out := make([]byte, HeaderSize+len(plaintext))
	copy(out, headerBytes[:])
	if len(plaintext) > 0 {
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return nil, err
		}
		stream := cipher.NewCTR(block, headerBytes[:])
		stream.XORKeyStream(out[HeaderSize:], plaintext)
	}
	if len(out) > limits.MaxUDPDatagramBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, len(out), limits.MaxUDPDatagramBytes)
	}
	return out, nil
}

// Decrypt validates framing, then decrypts the payload using the full packet
// header as the AES-CTR initial counter. It deliberately does not attempt any
// integrity verification: AES-CTR has none, and synthesising one here would
// misrepresent the firmware wire format.
func Decrypt(key [16]byte, datagram []byte) (Header, []byte, error) {
	if len(datagram) < HeaderSize {
		return Header{}, nil, fmt.Errorf("%w: %d < %d", ErrPacketTooShort, len(datagram), HeaderSize)
	}
	if len(datagram) > limits.MaxUDPDatagramBytes {
		return Header{}, nil, fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, len(datagram), limits.MaxUDPDatagramBytes)
	}
	h, err := ParseHeader(datagram)
	if err != nil {
		return Header{}, nil, err
	}
	body := datagram[HeaderSize:]
	if int(h.PayloadLen) != len(body) {
		return Header{}, nil, fmt.Errorf("%w: declared %d actual %d", ErrLengthMismatch, h.PayloadLen, len(body))
	}
	plaintext := make([]byte, len(body))
	if len(body) > 0 {
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return Header{}, nil, err
		}
		headerBytes := h.Marshal()
		stream := cipher.NewCTR(block, headerBytes[:])
		stream.XORKeyStream(plaintext, body)
	}
	return h, plaintext, nil
}

// Template is the negotiated packet type/flags pair for one audio session.
// Frames whose type/flags do not match the template are rejected before any
// audio handling.
type Template struct {
	PacketType byte
	Flags      byte
}

// Matches reports whether h carries the negotiated template values.
func (t Template) Matches(h Header) bool {
	return h.PacketType == t.PacketType && h.Flags == t.Flags
}

// String is safe for logging (contains no key material).
func (t Template) String() string {
	return fmt.Sprintf("type=0x%02x flags=0x%02x", t.PacketType, t.Flags)
}
