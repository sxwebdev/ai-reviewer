package security

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactorLiteral(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("glpat-supersecrettoken123")
	got := r.Mask("token is glpat-supersecrettoken123 ok")
	if strings.Contains(got, "supersecret") {
		t.Errorf("literal not masked: %q", got)
	}
	if !strings.Contains(got, placeholder) {
		t.Errorf("placeholder missing: %q", got)
	}
}

func TestRedactorIgnoresShortLiteral(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("abc") // below minLiteralLen
	got := r.Mask("abc def abc")
	if got != "abc def abc" {
		t.Errorf("short literal should not be masked: %q", got)
	}
}

func TestRedactorBuiltinPatterns(t *testing.T) {
	r := NewRedactor()
	cases := map[string]string{
		"pat":    "glpat-ABCDEFGHIJKLMNOPQRST",
		"apikey": "sk-ant-ABCDEFGHIJKLMNOPQRST",
		"bearer": "Authorization: Bearer abcdef0123456789",
		"email":  "contact me at john.doe@example.com please",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := r.Mask(in)
			if !strings.Contains(got, placeholder) {
				t.Errorf("pattern %s not masked: %q", name, got)
			}
		})
	}
}

func TestRedactingHandlerMasksMessageAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{})
	// Use a dedicated redactor via the process-wide default.
	RegisterSecret("glpat-loghandlersecret99")
	log := slog.New(NewRedactingHandler(inner))

	log.Info("using token glpat-loghandlersecret99",
		slog.String("token", "glpat-loghandlersecret99"),
		slog.Group("nested", slog.String("inner", "glpat-loghandlersecret99")),
	)

	out := buf.String()
	if strings.Contains(out, "loghandlersecret") {
		t.Errorf("secret leaked into log output: %q", out)
	}
	if !strings.Contains(out, placeholder) {
		t.Errorf("expected placeholder in output: %q", out)
	}
}

func TestRedactingHandlerMasksNonStringAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{})
	RegisterSecret("glpat-nonstringsecret42")
	log := slog.New(NewRedactingHandler(inner))

	// A secret buried in a slice or struct (a common place: command args) must
	// not slip past the redactor.
	log.Info("cmd",
		slog.Any("args", []string{"--token", "glpat-nonstringsecret42"}),
		slog.Any("cfg", struct{ Token string }{Token: "glpat-nonstringsecret42"}),
	)
	out := buf.String()
	if strings.Contains(out, "nonstringsecret") {
		t.Errorf("secret leaked via non-string attr: %q", out)
	}
	if !strings.Contains(out, placeholder) {
		t.Errorf("expected placeholder: %q", out)
	}
}

func TestMaskingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewMaskingWriter(&buf)
	RegisterSecret("glpat-writersecret1234")
	n, err := w.Write([]byte("leak glpat-writersecret1234 end"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len("leak glpat-writersecret1234 end") {
		t.Errorf("Write returned n=%d, want original length", n)
	}
	if strings.Contains(buf.String(), "writersecret") {
		t.Errorf("masking writer leaked: %q", buf.String())
	}
}
