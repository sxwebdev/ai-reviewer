package security

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// memSink is the in-memory destination the assertions read back. It also counts
// Sync calls so the Sync delegation can be checked.
type memSink struct {
	buf   bytes.Buffer
	syncs atomic.Int64
}

func (s *memSink) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *memSink) Sync() error                 { s.syncs.Add(1); return nil }
func (s *memSink) String() string              { return s.buf.String() }

// newTestLogger wires a real zap logger through NewRedactingCore onto a JSON
// encoder — the same shape as the production wiring
// (logger.WithZapOption(zap.WrapCore(security.NewRedactingCore))), so the test
// exercises the encoder path rather than the wrapper in isolation.
func newTestLogger(t *testing.T, level zapcore.Level) (*zap.Logger, *memSink) {
	t.Helper()
	sink := &memSink{}
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := NewRedactingCore(zapcore.NewCore(enc, sink, level))
	return zap.New(core), sink
}

// assertNoSecret checks the distinctive middle of the secret, not the whole
// literal: a partial mask would still be a leak.
func assertNoSecret(t *testing.T, out, fragment string) {
	t.Helper()
	if strings.Contains(out, fragment) {
		t.Errorf("secret fragment %q leaked into log output: %s", fragment, out)
	}
	if !strings.Contains(out, placeholder) {
		t.Errorf("expected %s in output: %s", placeholder, out)
	}
}

type secretHolder struct {
	Token string
}

// MarshalLogObject makes secretHolder a zapcore.ObjectMarshaler, so zap.Object
// encodes it natively instead of reflecting over it.
func (s secretHolder) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("token", s.Token)
	return nil
}

func TestRedactingCoreMasksMessageAndFields(t *testing.T) {
	const secret = "glpat-zapcoresecretvalue1"
	RegisterSecret(secret)

	log, sink := newTestLogger(t, zapcore.DebugLevel)
	log.Info("using token "+secret,
		zap.String("token", secret),
		zap.Namespace("nested"),
		zap.String("inner", secret),
	)

	assertNoSecret(t, sink.String(), "zapcoresecretvalue")
}

func TestRedactingCoreMasksErrorField(t *testing.T) {
	const secret = "glpat-zapcoreerrsecret456"
	RegisterSecret(secret)

	log, sink := newTestLogger(t, zapcore.DebugLevel)
	// Wrapped, the way a transport error carrying a header would arrive.
	err := errors.New("request failed with PRIVATE-TOKEN " + secret)
	log.Error("gitlab call failed", zap.Error(err))

	assertNoSecret(t, sink.String(), "zapcoreerrsecret")
}

func TestRedactingCoreMasksNonStringFields(t *testing.T) {
	const secret = "glpat-zapcorenonstring789"
	RegisterSecret(secret)

	log, sink := newTestLogger(t, zapcore.DebugLevel)
	// The three shapes a secret realistically hides in: a command's argv, a
	// reflected config struct, and a type that marshals itself.
	log.Info("cmd",
		zap.Strings("args", []string{"--token", secret}),
		zap.Any("cfg", struct{ Token string }{Token: secret}),
		zap.Object("holder", secretHolder{Token: secret}),
	)

	assertNoSecret(t, sink.String(), "zapcorenonstring")
}

// TestRedactingCoreMasksStringerFields covers zapcore.StringerType, which is
// the shape the production logger produces most often and the one the other
// non-string tests miss: mx's sugared Infow(msg, k, v) routes every value
// through zap.Any, and zap.Any returns a Stringer field for anything
// implementing fmt.Stringer. A *url.URL with userinfo is the realistic carrier —
// String() renders the password in full, and Redacted() is never called for us.
func TestRedactingCoreMasksStringerFields(t *testing.T) {
	const secret = "glpat-zapcorestringer0042"
	RegisterSecret(secret)

	u, err := url.Parse("postgres://ai_reviewer:" + secret + "@db:5432/app")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("zap.Any", func(t *testing.T) {
		log, sink := newTestLogger(t, zapcore.DebugLevel)
		log.Info("connecting", zap.Any("dsn", u))
		assertNoSecret(t, sink.String(), "zapcorestringer")
	})

	t.Run("zap.Stringer", func(t *testing.T) {
		log, sink := newTestLogger(t, zapcore.DebugLevel)
		log.Info("connecting", zap.Stringer("dsn", u))
		assertNoSecret(t, sink.String(), "zapcorestringer")
	})

	t.Run("sugared Infow, the way mx logs", func(t *testing.T) {
		log, sink := newTestLogger(t, zapcore.DebugLevel)
		log.Sugar().Infow("connecting", "dsn", u)
		assertNoSecret(t, sink.String(), "zapcorestringer")
	})
}

func TestRedactingCoreMasksWithFieldsAtAttachTime(t *testing.T) {
	const secret = "glpat-zapcorewithsecret22"
	RegisterSecret(secret)

	log, sink := newTestLogger(t, zapcore.DebugLevel)
	// A logger built once and reused would otherwise leak on every line.
	child := log.With(zap.String("token", secret), zap.Strings("argv", []string{secret}))
	child.Info("first")
	child.Info("second")

	out := sink.String()
	assertNoSecret(t, out, "zapcorewithsecret")
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected two log lines, got: %s", out)
	}
}

// TestRedactingCoreKeepsOrdinaryValues is the counterpart guarantee: a value
// with no secret in it must reach the encoder untouched, keeping its native
// type. Substituting unconditionally would turn every struct field into a
// fmt.Sprint string and make JSON logs unparseable downstream.
func TestRedactingCoreKeepsOrdinaryValues(t *testing.T) {
	log, sink := newTestLogger(t, zapcore.DebugLevel)
	log.Info("scan finished",
		zap.Any("cfg", struct{ Team string }{Team: "payments"}),
		zap.Int("mr_iid", 42),
		zap.Strings("repositories", []string{"backend/payments"}),
	)

	out := sink.String()
	for _, want := range []string{
		`"cfg":{"Team":"payments"}`, // still an object, not a rendered string
		`"mr_iid":42`,               // still a number
		`"repositories":["backend/payments"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s in output, got: %s", want, out)
		}
	}
	if strings.Contains(out, placeholder) {
		t.Errorf("nothing should have been masked here: %s", out)
	}
}

func TestRedactingCoreRespectsLevel(t *testing.T) {
	log, sink := newTestLogger(t, zapcore.ErrorLevel)

	log.Info("dropped")
	if sink.String() != "" {
		t.Fatalf("Check must not pass a sub-threshold entry through: %s", sink.String())
	}

	log.Error("kept")
	if !strings.Contains(sink.String(), "kept") {
		t.Errorf("error-level entry missing: %s", sink.String())
	}
}

func TestRedactingCoreSyncDelegates(t *testing.T) {
	log, sink := newTestLogger(t, zapcore.DebugLevel)
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := sink.syncs.Load(); got != 1 {
		t.Errorf("inner Sync called %d times, want 1", got)
	}
}

// TestRedactingCoreIsWrapCoreCompatible pins the signature the production
// wiring depends on: zap.WrapCore(security.NewRedactingCore).
func TestRedactingCoreIsWrapCoreCompatible(t *testing.T) {
	const secret = "glpat-zapcorewrapcore3344"
	RegisterSecret(secret)

	sink := &memSink{}
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	base := zap.New(zapcore.NewCore(enc, sink, zapcore.DebugLevel))

	wrapped := base.WithOptions(zap.WrapCore(NewRedactingCore))
	wrapped.Info("token " + secret)

	assertNoSecret(t, sink.String(), "zapcorewrapcore")
}
