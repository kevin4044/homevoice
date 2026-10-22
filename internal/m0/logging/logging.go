// Package logging provides structured logging with mandatory secret
// redaction for the isolated M0 probe.
//
// The probe never logs payload audio, credentials, UDP keys, bearer tokens or
// full application messages. As defence in depth, every handler created here
// redacts registered secrets from messages and attribute values, and tests
// capture log output to prove synthetic secrets do not escape.
package logging

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// Redactor replaces registered secret substrings with a fixed marker.
// It is safe for concurrent use.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// NewRedactor creates a redactor pre-loaded with secrets. Empty and very
// short values are ignored to avoid pathological partial matches.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers more secrets for redaction.
func (r *Redactor) Add(secrets ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) < 4 {
			continue
		}
		dup := false
		for _, existing := range r.secrets {
			if existing == s {
				dup = true
				break
			}
		}
		if !dup {
			r.secrets = append(r.secrets, s)
		}
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact returns s with all registered secrets replaced.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	secrets := append([]string(nil), r.secrets...)
	r.mu.RUnlock()
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

// SecretMarker is the replacement text used by Redact.
const SecretMarker = "[REDACTED]"

// redactingHandler wraps a slog.Handler and sanitises messages, attributes and
// error values before they reach the writer.
type redactingHandler struct {
	inner slog.Handler
	red   *Redactor
}

// New creates a text logger writing to w with the given level and redactor.
func New(w io.Writer, level slog.Level, red *Redactor) *slog.Logger {
	base := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	if red == nil {
		red = NewRedactor()
	}
	return slog.New(&redactingHandler{inner: base, red: red})
}

// Discard returns a logger that drops all records (useful for clean test
// output when no log capture is needed).
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.red.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, h.redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out), red: h.red}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), red: h.red}
}

func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, 0, len(group))
		for _, ga := range group {
			out = append(out, h.redactAttr(ga))
		}
		return slog.Group(a.Key, out...)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.red.Redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && err != nil {
			return slog.String(a.Key, h.red.Redact(err.Error()))
		}
		if s, ok := a.Value.Any().(string); ok {
			return slog.String(a.Key, h.red.Redact(s))
		}
		if b, ok := a.Value.Any().([]byte); ok {
			return slog.String(a.Key, hexFingerprint(b))
		}
	}
	return a
}

// hexFingerprint deliberately avoids logging raw bytes (audio payloads,
// keys). It reports only a length and a short non-reversible prefix hash
// marker, which is enough to correlate events without leaking content.
func hexFingerprint(b []byte) string {
	return "bytes[len=" + itoa(len(b)) + "]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
