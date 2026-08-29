package audit

// Package-level wire protocol for the unix socket between cmd/nexusd
// (client, via SignerClient in signer.go) and cmd/signerd (server): one
// newline-delimited JSON request per connection, one newline-delimited JSON
// response back. A local unix socket makes anything richer than this
// (framing, multiplexing, a real RPC codec) unneeded complexity — see
// signer.go's doc comment on why a fresh dial per call is fine here too.
//
// This file has zero dependency on internal/audit/signerkey — both
// cmd/nexusd and cmd/signerd import it, and only cmd/signerd may also
// import signerkey (task 5.1's "nexusd can sign but cannot read the key").

// OpSign asks signerd to sign a 32-byte digest. OpPublicKey asks for the
// current signing key's public half, so nexusd's own internal/audit/
// verify.go can verify chain/anchor signatures without ever touching the
// private key.
const (
	OpSign      = "sign"
	OpPublicKey = "public_key"
)

// Request is one line sent to signerd. Exported (unlike this package's own
// internal types) because cmd/signerd — a different package — must encode/
// decode the exact same wire shape SignerClient does.
type Request struct {
	Op     string `json:"op"`
	Digest []byte `json:"digest,omitempty"` // base64 via encoding/json's []byte handling
}

// Response is one line sent back. Error is set (and every other field
// empty) on failure — the connection itself always closes cleanly either
// way, so a caller only needs to check Error, never distinguish a transport
// failure from an application one at this layer.
type Response struct {
	Signature []byte `json:"signature,omitempty"`
	PublicKey []byte `json:"public_key,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	Error     string `json:"error,omitempty"`
}
