package llm

import "testing"

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{name: "plain", in: `{"a":1}`, want: `{"a":1}`},
		{name: "wrapped in prose", in: "Here is the result:\n{\"a\":1}\nThanks!", want: `{"a":1}`},
		{name: "code fence", in: "```json\n{\"x\": {\"y\": 2}}\n```", want: `{"x": {"y": 2}}`},
		{name: "prose brace before json", in: `Use {curly} placeholder. Result: {"ok":true}`, want: `{"ok":true}`},
		{name: "prose string brace before json", in: `say "a { b" then {"a":1}`, want: `{"a":1}`},
		{name: "brace in string", in: `{"body":"use } carefully","n":1}`, want: `{"body":"use } carefully","n":1}`},
		{name: "escaped quote in string", in: `{"body":"say \"hi\" }","n":1}`, want: `{"body":"say \"hi\" }","n":1}`},
		{name: "nested", in: `prefix {"a":{"b":{"c":3}}} suffix`, want: `{"a":{"b":{"c":3}}}`},
		{name: "no object", in: `no json here`, err: true},
		{name: "unbalanced", in: `{"a": 1`, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractJSONObject(tt.in)
			if tt.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
