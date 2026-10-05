package security

import (
	"fmt"

	"go.uber.org/zap/zapcore"
)

// redactingCore wraps a zapcore.Core and masks secrets in the entry message,
// the stack trace, and every field value before they reach the encoder. It is
// the zap counterpart of RedactingHandler and keeps the same semantics: string
// values are masked, structured values are recursed into, and arbitrary values
// are rendered and substituted only when masking actually changed them.
//
// Wire it up with zap.WrapCore(security.NewRedactingCore) so it sits between
// the logger and the encoder — every log line then passes this choke point,
// including ones written by libraries that never heard of this package.
type redactingCore struct {
	inner zapcore.Core
	r     *Redactor
}

// NewRedactingCore wraps inner with the process-wide redactor. The signature
// matches zap.WrapCore's expected func(zapcore.Core) zapcore.Core.
func NewRedactingCore(inner zapcore.Core) zapcore.Core {
	return &redactingCore{inner: inner, r: defaultRedactor}
}

// Enabled implements zapcore.LevelEnabler.
func (c *redactingCore) Enabled(l zapcore.Level) bool { return c.inner.Enabled(l) }

// With masks the fields at the moment they are attached, mirroring
// RedactingHandler.WithAttrs: a logger built once with a secret-bearing field
// must not leak it on every subsequent line.
func (c *redactingCore) With(fields []zapcore.Field) zapcore.Core {
	return &redactingCore{inner: c.inner.With(c.maskFields(fields)), r: c.r}
}

// Check adds *this* core to the checked entry, not the inner one — otherwise
// zap would call inner.Write directly and skip redaction entirely.
func (c *redactingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.inner.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

// Write masks the entry and its fields, then delegates.
func (c *redactingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	ent.Message = c.r.Mask(ent.Message)
	// A stack trace can carry a secret in a frame's arguments or in a wrapped
	// error's text, and it is attached by zap, not by the call site.
	ent.Stack = c.r.Mask(ent.Stack)
	return c.inner.Write(ent, c.maskFields(fields))
}

// Sync implements zapcore.Core.
func (c *redactingCore) Sync() error { return c.inner.Sync() }

func (c *redactingCore) maskFields(fields []zapcore.Field) []zapcore.Field {
	if len(fields) == 0 {
		return fields
	}
	out := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		out[i] = c.maskField(f)
	}
	return out
}

// maskField masks one field. Numeric, boolean and time fields cannot carry a
// secret, so they pass through untouched — that keeps the hot path cheap and
// preserves the encoder's native typing.
func (c *redactingCore) maskField(f zapcore.Field) zapcore.Field {
	switch f.Type {
	case zapcore.StringType:
		f.String = c.r.Mask(f.String)
		return f

	case zapcore.ByteStringType:
		// UTF-8 text carried as bytes; BinaryType is deliberately left alone
		// since it is base64-encoded and a literal match cannot survive it.
		if b, ok := f.Interface.([]byte); ok {
			f.Interface = []byte(c.r.Mask(string(b)))
		}
		return f

	case zapcore.ErrorType:
		// Always substituted, exactly as the slog handler does: an error's
		// verbose form (stack, wrapped chain) is rendered by the encoder and
		// would otherwise bypass masking.
		if err, ok := f.Interface.(error); ok {
			return stringField(f.Key, c.r.Mask(err.Error()))
		}
		return f

	case zapcore.ObjectMarshalerType, zapcore.InlineMarshalerType:
		// Recurse: the marshaler writes into an encoder, so wrap the encoder
		// rather than the value. Nested namespaces and objects are covered
		// because the wrapper re-wraps on the way down.
		if om, ok := f.Interface.(zapcore.ObjectMarshaler); ok {
			f.Interface = maskingObject{inner: om, r: c.r}
		}
		return f

	case zapcore.ArrayMarshalerType:
		// zap.Strings and friends land here — a secret in a command's args must
		// not slip past just because it is one element of a slice.
		if am, ok := f.Interface.(zapcore.ArrayMarshaler); ok {
			f.Interface = maskingArray{inner: am, r: c.r}
		}
		return f

	case zapcore.ReflectType, zapcore.StringerType:
		// Arbitrary values (structs, maps, Stringers). Render and compare: only
		// substitute when masking changed something, so an ordinary struct keeps
		// its native encoding instead of collapsing into a fmt.Sprint string.
		return c.maskRendered(f)

	default:
		return f
	}
}

// maskRendered replaces f with a masked string form only if rendering it
// exposes a secret.
func (c *redactingCore) maskRendered(f zapcore.Field) zapcore.Field {
	s := fmt.Sprint(f.Interface)
	if masked := c.r.Mask(s); masked != s {
		return stringField(f.Key, masked)
	}
	return f
}

// stringField builds a zapcore string field without importing zap itself,
// keeping this package's dependency at zapcore.
func stringField(key, val string) zapcore.Field {
	return zapcore.Field{Key: key, Type: zapcore.StringType, String: val}
}

// maskingObject wraps an ObjectMarshaler so its output is masked as it is
// written.
type maskingObject struct {
	inner zapcore.ObjectMarshaler
	r     *Redactor
}

func (m maskingObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	return m.inner.MarshalLogObject(maskingObjectEncoder{ObjectEncoder: enc, r: m.r})
}

// maskingArray is the ArrayMarshaler counterpart.
type maskingArray struct {
	inner zapcore.ArrayMarshaler
	r     *Redactor
}

func (m maskingArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	return m.inner.MarshalLogArray(maskingArrayEncoder{ArrayEncoder: enc, r: m.r})
}

// maskingObjectEncoder embeds the real encoder and overrides only the methods
// that can carry text. Everything else (ints, bools, times, namespaces) is
// promoted unchanged, so the wrapper survives new methods on the interface.
type maskingObjectEncoder struct {
	zapcore.ObjectEncoder
	r *Redactor
}

func (e maskingObjectEncoder) AddString(key, val string) {
	e.ObjectEncoder.AddString(key, e.r.Mask(val))
}

func (e maskingObjectEncoder) AddByteString(key string, val []byte) {
	e.ObjectEncoder.AddByteString(key, []byte(e.r.Mask(string(val))))
}

func (e maskingObjectEncoder) AddObject(key string, om zapcore.ObjectMarshaler) error {
	return e.ObjectEncoder.AddObject(key, maskingObject{inner: om, r: e.r})
}

func (e maskingObjectEncoder) AddArray(key string, am zapcore.ArrayMarshaler) error {
	return e.ObjectEncoder.AddArray(key, maskingArray{inner: am, r: e.r})
}

func (e maskingObjectEncoder) AddReflected(key string, val any) error {
	s := fmt.Sprint(val)
	if masked := e.r.Mask(s); masked != s {
		e.ObjectEncoder.AddString(key, masked)
		return nil
	}
	return e.ObjectEncoder.AddReflected(key, val)
}

type maskingArrayEncoder struct {
	zapcore.ArrayEncoder
	r *Redactor
}

func (e maskingArrayEncoder) AppendString(val string) {
	e.ArrayEncoder.AppendString(e.r.Mask(val))
}

func (e maskingArrayEncoder) AppendByteString(val []byte) {
	e.ArrayEncoder.AppendByteString([]byte(e.r.Mask(string(val))))
}

func (e maskingArrayEncoder) AppendObject(om zapcore.ObjectMarshaler) error {
	return e.ArrayEncoder.AppendObject(maskingObject{inner: om, r: e.r})
}

func (e maskingArrayEncoder) AppendArray(am zapcore.ArrayMarshaler) error {
	return e.ArrayEncoder.AppendArray(maskingArray{inner: am, r: e.r})
}

func (e maskingArrayEncoder) AppendReflected(val any) error {
	s := fmt.Sprint(val)
	if masked := e.r.Mask(s); masked != s {
		e.ArrayEncoder.AppendString(masked)
		return nil
	}
	return e.ArrayEncoder.AppendReflected(val)
}
