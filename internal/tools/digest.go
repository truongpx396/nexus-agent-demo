package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalDigest is the one artifact task 3.5 names, with three jobs:
// approval binding, idempotency key, and step 8's re-verification after a
// hook rewrites input. It hashes ONLY {tool_id, input} — never output, never
// a timestamp — so the same intended call always binds to the same digest,
// across retries and across a human reviewing it later.
//
// Canonicalization here is a simplified RFC 8785 JCS: object keys sorted,
// no insignificant whitespace, strings re-escaped through encoding/json.
// Numbers are re-emitted via their original decoded token (json.Number)
// rather than JCS's own ECMA-262 number-to-string rule — stricter than JCS
// (byte-identical to the source token, never a reformatted one), so it
// still gives the "same input twice, same digest" property this system
// needs without implementing JCS's numeric-formatting edge cases.
func CanonicalDigest(toolID string, input json.RawMessage) ([]byte, error) {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	canonInput, err := canonicalizeJSON(input)
	if err != nil {
		return nil, fmt.Errorf("tools: canonicalize input: %w", err)
	}
	toolIDJSON, err := json.Marshal(toolID)
	if err != nil {
		return nil, fmt.Errorf("tools: marshal tool_id: %w", err)
	}
	payload := fmt.Sprintf(`{"input":%s,"tool_id":%s}`, canonInput, toolIDJSON)
	sum := sha256.Sum256([]byte(payload))
	return sum[:], nil
}

func canonicalizeJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(val.String())
	case string:
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("tools: cannot canonicalize value of type %T", v)
	}
	return nil
}
