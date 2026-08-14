package dbtypes_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
)

func TestJSONScan(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  []byte
	}{
		{name: "nil becomes nil", value: nil, want: nil},
		{name: "bytes are kept verbatim", value: []byte(`{"a": 1}`), want: []byte(`{"a": 1}`)},
		{name: "strings are kept verbatim", value: `{"a": 1}`, want: []byte(`{"a": 1}`)},
		{
			// Whitespace inside a string value is data, not formatting.
			name:  "significant whitespace survives",
			value: []byte(`{"cron":"0 9 * * *"}`),
			want:  []byte(`{"cron":"0 9 * * *"}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var j dbtypes.JSON
			if err := j.Scan(tt.value); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if !bytes.Equal(j, tt.want) {
				t.Errorf("got %q, want %q", j, tt.want)
			}
		})
	}
}

func TestJSONScanRejectsOtherTypes(t *testing.T) {
	t.Parallel()
	var j dbtypes.JSON
	if err := j.Scan(42); err == nil {
		t.Fatal("Scan(int) succeeded, want an error")
	}
}

// Scan must overwrite, not append: a reused struct is the normal case when a
// caller scans row after row into the same value.
func TestJSONScanOverwrites(t *testing.T) {
	t.Parallel()
	j := dbtypes.JSON(`{"old":true}`)
	if err := j.Scan([]byte(`{"new":1}`)); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if string(j) != `{"new":1}` {
		t.Errorf("got %q, want {\"new\":1}", j)
	}
}

func TestJSONValue(t *testing.T) {
	tests := []struct {
		name    string
		in      dbtypes.JSON
		want    any
		wantErr bool
	}{
		{name: "empty becomes SQL NULL", in: dbtypes.JSON{}, want: nil},
		{name: "nil becomes SQL NULL", in: nil, want: nil},
		{name: "object", in: dbtypes.JSON(`{"a":1}`), want: `{"a":1}`},
		{name: "empty object", in: dbtypes.EmptyObject(), want: `{}`},
		{name: "malformed payload is refused", in: dbtypes.JSON(`{`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.in.Value()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Value() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Value: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// EmptyObject is what the NOT NULL jsonb columns need: the zero value maps to
// SQL NULL and would violate the constraint.
func TestEmptyObjectIsNotNull(t *testing.T) {
	t.Parallel()
	v, err := dbtypes.EmptyObject().Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v == nil {
		t.Fatal("EmptyObject() maps to SQL NULL")
	}
	if zero, _ := (dbtypes.JSON{}).Value(); zero != nil {
		t.Fatalf("the zero JSON maps to %#v, want SQL NULL", zero)
	}
}

func TestMarshal(t *testing.T) {
	t.Parallel()
	got, err := dbtypes.Marshal(map[string]int{"a": 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q, want {\"a\":1}", got)
	}
	if _, err := dbtypes.Marshal(make(chan int)); err == nil {
		t.Error("Marshal(chan) succeeded, want an error")
	}
}

// JSON is embedded in API responses, so it must encode as the payload itself
// rather than as a base64 byte slice.
func TestJSONRoundTripsThroughEncodingJSON(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		Payload dbtypes.JSON `json:"payload"`
	}

	out, err := json.Marshal(wrapper{Payload: dbtypes.JSON(`{"blocks":[]}`)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != `{"payload":{"blocks":[]}}` {
		t.Fatalf("got %s, want {\"payload\":{\"blocks\":[]}}", out)
	}

	var back wrapper
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(back.Payload) != `{"blocks":[]}` {
		t.Errorf("got %q, want {\"blocks\":[]}", back.Payload)
	}

	// An empty payload must still produce valid JSON.
	out, err = json.Marshal(wrapper{})
	if err != nil {
		t.Fatalf("Marshal empty: %v", err)
	}
	if string(out) != `{"payload":null}` {
		t.Errorf("got %s, want {\"payload\":null}", out)
	}
}

func TestJSONUnmarshalNilPointer(t *testing.T) {
	t.Parallel()
	var j *dbtypes.JSON
	if err := j.UnmarshalJSON([]byte(`{}`)); err == nil {
		t.Fatal("UnmarshalJSON on a nil pointer succeeded, want an error")
	}
}
