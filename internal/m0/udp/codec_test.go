package udp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

func vectorByID(t *testing.T, id string) fixtures.UDPVector {
	t.Helper()
	suite, err := fixtures.LoadUDPVectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range suite.Vectors {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("vector %q not found", id)
	return fixtures.UDPVector{}
}

func keyFromHex(t *testing.T, s string) [16]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad key %q: %v", s, err)
	}
	var k [16]byte
	copy(k[:], raw)
	return k
}

func headerFromVector(v fixtures.UDPVector) Header {
	return Header{
		PacketType: byte(v.Header.PacketType),
		Flags:      byte(v.Header.Flags),
		PayloadLen: uint16(v.Header.PayloadLen),
		SSRC:       v.Header.SSRC,
		Timestamp:  v.Header.Timestamp,
		Sequence:   v.Header.Sequence,
	}
}

// TestA08PublicVectorExactMatch proves the public AES-CTR vector matches
// byte-for-byte, that decryption uses the packet header rather than a fixed
// nonce, and that type/flags template bytes are preserved.
func TestA08PublicVectorExactMatch(t *testing.T) {
	v := vectorByID(t, "valid-20-byte-payload")
	key := keyFromHex(t, v.KeyHex)
	plaintext, _ := hex.DecodeString(v.PlaintextHex)
	wantPacket, _ := hex.DecodeString(v.PacketHex)

	got, err := Encrypt(key, headerFromVector(v), plaintext)
	if err != nil {
		t.Fatalf("Encrypt = %v", err)
	}
	if !bytes.Equal(got, wantPacket) {
		t.Fatalf("encrypted packet mismatch\n got %x\nwant %x", got, wantPacket)
	}

	h, decrypted, err := Decrypt(key, wantPacket)
	if err != nil {
		t.Fatalf("Decrypt = %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted plaintext mismatch\n got %x\nwant %x", decrypted, plaintext)
	}
	if h.PacketType != 0x01 || h.Flags != 0xa0 {
		t.Fatalf("template not preserved: type=%#x flags=%#x", h.PacketType, h.Flags)
	}

	// The documented HELLO base nonce is NOT the per-packet counter. Using it
	// as a fixed IV must not reproduce the plaintext; this catches an
	// implementation that ignores the header.
	baseNonce, err := hex.DecodeString(v.BaseNonceHex)
	if err != nil || len(baseNonce) != 16 {
		t.Fatalf("bad base nonce in fixture: %v", err)
	}
	block, _ := aes.NewCipher(key[:])
	fixedIV := make([]byte, len(wantPacket)-HeaderSize)
	cipher.NewCTR(block, baseNonce).XORKeyStream(fixedIV, wantPacket[HeaderSize:])
	if bytes.Equal(fixedIV, plaintext) {
		t.Fatal("decryption with the fixed HELLO nonce matched plaintext; header is not being used as the counter")
	}
}

// TestA13TamperedCiphertextHasNoAuthentication proves the documented AES-CTR
// limitation: tampering changes plaintext but produces no authentication
// error. The probe must never claim UDP integrity protection.
func TestA13TamperedCiphertextHasNoAuthentication(t *testing.T) {
	v := vectorByID(t, "tampered-ciphertext")
	key := keyFromHex(t, v.KeyHex)
	packet, _ := hex.DecodeString(v.PacketHex)
	wantPlain, _ := hex.DecodeString(v.ExpectedPlaintextHex)

	h, plain, err := Decrypt(key, packet)
	if err != nil {
		t.Fatalf("tampered ciphertext was rejected with %v; AES-CTR has no authentication tag and "+
			"this implementation must report the limitation instead of inventing one", err)
	}
	if !bytes.Equal(plain, wantPlain) {
		t.Fatalf("tampered plaintext mismatch\n got %x\nwant %x", plain, wantPlain)
	}
	if bytes.Equal(plain, mustHex(t, vectorByID(t, "valid-20-byte-payload").PlaintextHex)) {
		t.Fatal("tampered ciphertext produced pristine plaintext")
	}
	if h.PayloadLen != 20 {
		t.Fatalf("payload length = %d", h.PayloadLen)
	}
	// Same key/header reuse is a known CTR risk documented for the report.
	// It is not solved by this codec.
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestA09CodecRejectsMalformedFraming covers framing/length bounds at the
// codec level.
func TestA09CodecRejectsMalformedFraming(t *testing.T) {
	key := keyFromHex(t, "00112233445566778899aabbccddeeff")

	short, _ := hex.DecodeString(vectorByID(t, "short-header").PacketHex)
	if _, _, err := Decrypt(key, short); !errors.Is(err, ErrPacketTooShort) {
		t.Fatalf("short header error = %v", err)
	}

	truncated, _ := hex.DecodeString(vectorByID(t, "truncated-payload").PacketHex)
	if _, _, err := Decrypt(key, truncated); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("truncated payload error = %v", err)
	}

	oversize := make([]byte, limits.MaxUDPDatagramBytes+1)
	if _, _, err := Decrypt(key, oversize); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("oversize datagram error = %v", err)
	}

	bigPayload := make([]byte, limits.MaxUDPPayloadBytes+1)
	if _, err := Encrypt(key, Header{PacketType: 1}, bigPayload); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversize payload error = %v", err)
	}

	// Zero-length payload is valid at the codec layer (used for pure
	// envelope tests) but callers must not feed it to an audio decoder.
	empty, err := Encrypt(key, Header{PacketType: 1, SSRC: 7, Sequence: 1}, nil)
	if err != nil {
		t.Fatalf("Encrypt(empty) = %v", err)
	}
	h, payload, err := Decrypt(key, empty)
	if err != nil {
		t.Fatalf("Decrypt(empty) = %v", err)
	}
	if h.PayloadLen != 0 || len(payload) != 0 {
		t.Fatalf("empty payload decode = len %d/%d", h.PayloadLen, len(payload))
	}
	if len(empty) != HeaderSize {
		t.Fatalf("empty packet length = %d, want %d", len(empty), HeaderSize)
	}
}

func TestCodecExactMaxPayload(t *testing.T) {
	key := keyFromHex(t, "00112233445566778899aabbccddeeff")
	payload := make([]byte, limits.MaxUDPPayloadBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	packet, err := Encrypt(key, Header{PacketType: 1}, payload)
	if err != nil {
		t.Fatalf("Encrypt(max payload) = %v", err)
	}
	if len(packet) != limits.MaxUDPDatagramBytes {
		t.Fatalf("packet len = %d, want %d", len(packet), limits.MaxUDPDatagramBytes)
	}
	_, got, err := Decrypt(key, packet)
	if err != nil {
		t.Fatalf("Decrypt(max payload) = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("max payload round trip mismatch")
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{PacketType: 3, Flags: 7, PayloadLen: 1400, SSRC: 0xdeadbeef, Timestamp: 0x12345678, Sequence: 0xffffffff}
	b := h.Marshal()
	got, err := ParseHeader(b[:])
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("header round trip = %+v, want %+v", got, h)
	}
}

func TestTemplateMatches(t *testing.T) {
	tpl := Template{PacketType: 1, Flags: 0}
	if !tpl.Matches(Header{PacketType: 1, Flags: 0, SSRC: 9}) {
		t.Fatal("valid template did not match")
	}
	if tpl.Matches(Header{PacketType: 1, Flags: 0xa0}) {
		t.Fatal("flags mismatch accepted")
	}
	if tpl.Matches(Header{PacketType: 2, Flags: 0}) {
		t.Fatal("type mismatch accepted")
	}
}
