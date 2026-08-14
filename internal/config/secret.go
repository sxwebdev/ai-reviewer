package config

// Secret is a string config value that never leaks into logs, YAML/JSON dumps
// or %v formatting. Use Unmask() at the single point where the real value is
// handed to a client library.
//
// Secret fields carry `secret:"true" vault:"true"`: the first marks them for
// xconfig's own redaction, the second sources them from Vault (which reads the
// key from the field's env name — see the env-tag rule in config.go).
type Secret string

const redactedPlaceholder = "[redacted]"

// Unmask returns the real secret value.
func (s Secret) Unmask() string { return string(s) }

// IsSet reports whether a value was provided.
func (s Secret) IsSet() bool { return s != "" }

// String implements fmt.Stringer — %s/%v print "[redacted]".
func (s Secret) String() string { return redactedPlaceholder }

// GoString implements fmt.GoStringer — %#v prints "[redacted]".
func (s Secret) GoString() string { return redactedPlaceholder }

// MarshalJSON hides the value in JSON dumps.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redactedPlaceholder + `"`), nil
}

// MarshalYAML hides the value in YAML dumps (goccy/go-yaml InterfaceMarshaler).
func (s Secret) MarshalYAML() (any, error) {
	return redactedPlaceholder, nil
}
