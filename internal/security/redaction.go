package security

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// RedactingHandler wraps a slog.Handler and masks secrets in the log message
// and in every string attribute value (recursing into groups) before they are
// written. Preset attributes added via WithAttrs are redacted at set time.
type RedactingHandler struct {
	inner slog.Handler
	r     *Redactor
}

// NewRedactingHandler wraps inner with the process-wide redactor.
func NewRedactingHandler(inner slog.Handler) *RedactingHandler {
	return &RedactingHandler{inner: inner, r: defaultRedactor}
}

// Enabled implements slog.Handler.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler, rebuilding the record with masked values.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.r.Mask(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

// WithAttrs implements slog.Handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = h.redactAttr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(redacted), r: h.r}
}

// WithGroup implements slog.Handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name), r: h.r}
}

// redactAttr masks a single attribute value, recursing into groups. It resolves
// LogValuer values first, and for arbitrary (KindAny) values — structs, slices
// like a command's args, etc. — it masks their rendered form so a secret buried
// in a non-string value cannot slip past the choke point.
func (h *RedactingHandler) redactAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve() // unwrap LogValuer
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.Mask(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, g := range group {
			out[i] = h.redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case string:
			return slog.String(a.Key, h.r.Mask(v))
		case error:
			return slog.String(a.Key, h.r.Mask(v.Error()))
		default:
			// Render structs/slices/maps and mask; only replace (losing the
			// original type) when masking actually changed something, so
			// non-secret values keep their formatting.
			s := fmt.Sprint(v)
			if masked := h.r.Mask(s); masked != s {
				return slog.String(a.Key, masked)
			}
			return a
		}
	default:
		return a
	}
}

// maskingWriter masks secrets in each Write before delegating.
type maskingWriter struct {
	w io.Writer
	r *Redactor
}

// NewMaskingWriter returns an io.Writer that masks secrets in everything
// written to it. Use it to wrap subprocess stdout/stderr captured for logs.
func NewMaskingWriter(w io.Writer) io.Writer {
	return &maskingWriter{w: w, r: defaultRedactor}
}

func (m *maskingWriter) Write(p []byte) (int, error) {
	masked := m.r.Mask(string(p))
	if _, err := m.w.Write([]byte(masked)); err != nil {
		return 0, err
	}
	// Report the original length so callers see a complete write.
	return len(p), nil
}
