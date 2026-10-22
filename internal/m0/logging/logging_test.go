package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactsMessagesAttributesAndErrors(t *testing.T) {
	var buf bytes.Buffer
	secret := "synthetic-password-9f3a2c"
	key := "00112233445566778899aabbccddeeff"
	logger := New(&buf, slog.LevelDebug, NewRedactor(secret, key))

	logger.Info("auth failed for "+secret, "secret", secret, "wrapped", errors.New("bad key "+key))
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("log output leaked password: %q", out)
	}
	if strings.Contains(out, key) {
		t.Fatalf("log output leaked key: %q", out)
	}
	if !strings.Contains(out, SecretMarker) {
		t.Fatalf("log output missing redaction marker: %q", out)
	}
}

func TestRedactsBytesAsLengthOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, NewRedactor())
	audio := []byte{0x01, 0x02, 0x03, 0x04}
	logger.Info("udp frame", "payload", audio)
	out := buf.String()
	if strings.Contains(out, "01020304") || strings.Contains(out, "\x01\x02") {
		t.Fatalf("log output leaked raw bytes: %q", out)
	}
	if !strings.Contains(out, "bytes[len=4]") {
		t.Fatalf("log output missing bounded bytes summary: %q", out)
	}
}

func TestRedactorAddAndShortSecrets(t *testing.T) {
	r := NewRedactor("abcd")
	if got := r.Redact("xxabcdyy"); got != "xx[REDACTED]yy" {
		t.Fatalf("Redact = %q", got)
	}
	r.Add("abc", "longer-secret") // 3-char values are ignored to avoid noise
	if got := r.Redact("abc longer-secret"); got != "abc [REDACTED]" {
		t.Fatalf("short secret handling = %q", got)
	}
	var nilR *Redactor
	if got := nilR.Redact("anything"); got != "anything" {
		t.Fatalf("nil Redactor changed input: %q", got)
	}
	nilR.Add("ignored") // must not panic
}

func TestWithAttrsStillRedacts(t *testing.T) {
	var buf bytes.Buffer
	secret := "synthetic-bearer-token-1234"
	logger := New(&buf, slog.LevelInfo, NewRedactor(secret)).With("component", secret)
	logger.Info("start", slog.String("detail", secret))
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("WithAttrs leaked secret: %q", out)
	}
	if !strings.Contains(out, SecretMarker) {
		t.Fatalf("with-attrs output missing redaction: %q", out)
	}
}
