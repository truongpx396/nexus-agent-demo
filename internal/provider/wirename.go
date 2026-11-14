package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// maxWireNameLen is the shortest tool-name limit among the wire protocols the
// adapters speak: Anthropic accepts 128 bytes, OpenAI-compatible APIs 64.
const maxWireNameLen = 64

// ErrWireNameCollision is returned by NewWireNames when two different tool
// references would be sent under the same wire name.
var ErrWireNameCollision = errors.New("provider: tool references collide on the wire")

// WireNames translates between a tool's internal reference
// (`namespace/name@version`, which the kernel, the event log and the tool
// pipeline all use) and the name an adapter sends to a hosted API, which only
// accepts letters, digits, `_` and `-`. The translation happens at the adapter
// boundary and nowhere else, so the rest of the system never sees a wire name.
//
// The zero value is valid: it maps nothing, so Wire falls back to the
// sanitized form of whatever it is given and Ref returns its argument as is.
type WireNames struct {
	toWire map[string]string
	toRef  map[string]string
}

// NewWireNames builds the translation for one request's tool catalog. It fails
// when two different references sanitize to the same wire name, because the
// model's reply could not then be mapped back to a single tool.
func NewWireNames(tools []ToolSchema) (WireNames, error) {
	w := WireNames{
		toWire: make(map[string]string, len(tools)),
		toRef:  make(map[string]string, len(tools)),
	}
	for _, t := range tools {
		wire := sanitizeWireName(t.Name)
		if prev, taken := w.toRef[wire]; taken && prev != t.Name {
			return WireNames{}, fmt.Errorf("%w: %q and %q are both sent as %q", ErrWireNameCollision, prev, t.Name, wire)
		}
		w.toWire[t.Name] = wire
		w.toRef[wire] = t.Name
	}
	return w, nil
}

// Wire returns the name to send for a tool reference. A reference that is not
// in the catalog (a replayed call to a tool that is no longer loaded) is
// sanitized the same way, so history stays well-formed on the wire.
func (w WireNames) Wire(ref string) string {
	if wire, ok := w.toWire[ref]; ok {
		return wire
	}
	return sanitizeWireName(ref)
}

// Ref returns the internal reference for a name the provider sent back, or the
// name unchanged when it is not one this request advertised.
func (w WireNames) Ref(wire string) string {
	if ref, ok := w.toRef[wire]; ok {
		return ref
	}
	return wire
}

// sanitizeWireName replaces every byte outside [a-zA-Z0-9_-] with '_'. A result
// longer than maxWireNameLen keeps its first 55 bytes, an underscore and the
// first 8 hex characters of the SHA-256 of the full reference, so two long
// references that share a prefix still differ.
func sanitizeWireName(ref string) string {
	var b strings.Builder
	b.Grow(len(ref))
	for i := range len(ref) {
		c := ref[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) <= maxWireNameLen {
		return out
	}
	sum := sha256.Sum256([]byte(ref))
	return out[:maxWireNameLen-9] + "_" + hex.EncodeToString(sum[:4])
}
