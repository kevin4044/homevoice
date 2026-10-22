package broker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/homevoice/homevoice/internal/m0/logging"
)

// newSafeLogger wraps target so that every record the broker or the MQTT
// library emits is sanitised before it reaches the final handler.
//
// The probe must never log credentials, keys or application payloads. The
// MQTT library logs decoded packets (including packet payloads) on some error
// paths, so a plain *slog.Logger is not enough: this handler replaces any
// opaque value (packets, byte slices, structs) with a type/length marker and
// applies the probe's secret redactor to every string and error.
func newSafeLogger(target *slog.Logger, secrets []string) *slog.Logger {
	if target == nil {
		target = logging.Discard()
	}
	return slog.New(&safeHandler{target: target, red: logging.NewRedactor(secrets...)})
}

// sanitizedPlaceholder marks an opaque attribute that was not logged verbatim.
const sanitizedPlaceholder = "[REDACTED]"

type safeHandler struct {
	target *slog.Logger
	red    *logging.Redactor
	attrs  []slog.Attr // pre-sanitised attributes from WithAttrs
	groups []string    // groups applied by WithGroup, outermost first
}

func (h *safeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.target.Enabled(ctx, level)
}

func (h *safeHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, r.NumAttrs()+len(h.attrs))
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, h.safeAttr(a))
		return true
	})
	// Rebuild groups from the inside out so the final handler sees the same
	// nesting that a normal handler would produce.
	for i := len(h.groups) - 1; i >= 0; i-- {
		attrs = []slog.Attr{slog.Group(h.groups[i], attrsToAny(attrs)...)}
	}
	h.target.LogAttrs(ctx, r.Level, h.red.Redact(r.Message), attrs...)
	return nil
}

func (h *safeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &safeHandler{
		target: h.target,
		red:    h.red,
		attrs:  make([]slog.Attr, 0, len(h.attrs)+len(attrs)),
		groups: h.groups,
	}
	next.attrs = append(next.attrs, h.attrs...)
	for _, a := range attrs {
		next.attrs = append(next.attrs, h.safeAttr(a))
	}
	return next
}

func (h *safeHandler) WithGroup(name string) slog.Handler {
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &safeHandler{target: h.target, red: h.red, attrs: h.attrs, groups: groups}
}

func (h *safeHandler) safeAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, 0, len(group))
		for _, ga := range group {
			out = append(out, h.safeAttr(ga))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindString:
		return slog.String(a.Key, h.red.Redact(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			return slog.String(a.Key, h.red.Redact(v.Error()))
		case string:
			return slog.String(a.Key, h.red.Redact(v))
		case []byte:
			// Length only: payloads and keys are never written out.
			return slog.String(a.Key, fmt.Sprintf("bytes[len=%d]", len(v)))
		default:
			return slog.String(a.Key, fmt.Sprintf("%s(%T)", sanitizedPlaceholder, v))
		}
	default:
		return a
	}
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
	}
	return out
}

// sanitizeReason bounds and redacts an operator/callback-provided reason so it
// cannot carry secrets and cannot grow unbounded.
func sanitizeReason(red *logging.Redactor, reason string, max int) string {
	// Redact before truncating: truncating first could cut a secret in half,
	// leaving an unredactable prefix behind.
	reason = strings.TrimSpace(red.Redact(reason))
	if max > 0 && len(reason) > max {
		reason = reason[:max]
	}
	return reason
}
