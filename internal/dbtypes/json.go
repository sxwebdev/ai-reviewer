// Package dbtypes holds custom SQL column types for Postgres.
package dbtypes

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSON stores a raw JSON payload for Postgres jsonb columns. Scan keeps the
// payload byte-for-byte — the columns behind it (Block Kit blocks, pipeline
// provenance, GitLab positions) are round-tripped, not interpreted, and
// whitespace inside string values is significant.
type JSON json.RawMessage

// EmptyObject is what the NOT NULL jsonb columns want when there is nothing to
// store. A zero JSON is not it: Value maps an empty payload to SQL NULL, which
// mr_reviews.pipeline_json, mr_reviews.risk_json and mr_findings.position_json
// reject.
func EmptyObject() JSON { return JSON(`{}`) }

// Marshal encodes v for a jsonb column.
func Marshal(v any) (JSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("dbtypes.Marshal: %w", err)
	}
	return JSON(b), nil
}

// MarshalJSON returns j as the JSON encoding of itself.
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON stores data verbatim.
func (j *JSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return fmt.Errorf("dbtypes.JSON: UnmarshalJSON on nil pointer")
	}
	*j = append((*j)[0:0], data...)
	return nil
}

// Scan implements sql.Scanner.
func (j *JSON) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*j = nil
	case string:
		*j = append((*j)[0:0], v...)
	case []byte:
		*j = append((*j)[0:0], v...)
	default:
		return fmt.Errorf("cannot scan %T into dbtypes.JSON", value)
	}
	return nil
}

// Value implements driver.Valuer.
func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("dbtypes.JSON: invalid JSON")
	}
	return string(j), nil
}
