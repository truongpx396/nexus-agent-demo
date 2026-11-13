package authn

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustKey(t *testing.T) (issuer *DevIssuer, verifier *DevVerifier) {
	t.Helper()
	priv, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	return NewDevIssuer(priv), NewDevVerifier(PublicKey(priv))
}

func TestDevIssuerVerifier_RoundTrip(t *testing.T) {
	issuer, verifier := mustKey(t)
	wantTenant, wantUser := uuid.New(), uuid.New()

	tok, err := issuer.Issue(wantTenant, wantUser, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	gotTenant, gotUser, err := verifier.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotTenant != wantTenant || gotUser != wantUser {
		t.Fatalf("Verify roundtrip = (%s,%s), want (%s,%s)", gotTenant, gotUser, wantTenant, wantUser)
	}
}

func TestDevVerifier_RejectsExpired(t *testing.T) {
	issuer, verifier := mustKey(t)
	tok, err := issuer.Issue(uuid.New(), uuid.New(), -time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := verifier.Verify(tok); err == nil {
		t.Fatal("Verify accepted an expired token")
	}
}

func TestDevVerifier_RejectsWrongKey(t *testing.T) {
	issuer, _ := mustKey(t)
	_, otherVerifier := mustKey(t)
	tok, err := issuer.Issue(uuid.New(), uuid.New(), time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := otherVerifier.Verify(tok); err == nil {
		t.Fatal("Verify accepted a token signed by a different key")
	}
}

func TestDevVerifier_RejectsGarbage(t *testing.T) {
	_, verifier := mustKey(t)
	if _, _, err := verifier.Verify("not-a-jwt"); err == nil {
		t.Fatal("Verify accepted a non-JWT string")
	}
	if _, _, err := verifier.Verify(""); err == nil {
		t.Fatal("Verify accepted an empty string")
	}
}

func TestDevVerifier_RejectsTamperedToken(t *testing.T) {
	issuer, verifier := mustKey(t)
	tok, err := issuer.Issue(uuid.New(), uuid.New(), time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Flip a character in the middle of the token (deep inside the payload
	// or signature segment, never the last char of the base64url signature
	// — those trailing bits can be padding-equivalent and a flip there may
	// decode to identical bytes, proving nothing about tamper detection).
	mid := len(tok) / 2
	flipped := byte('A')
	if tok[mid] == 'A' {
		flipped = 'B'
	}
	tampered := tok[:mid] + string(flipped) + tok[mid+1:]
	if _, _, err := verifier.Verify(tampered); err == nil {
		t.Fatal("Verify accepted a tampered token")
	}
}

func TestLoadPublicKey_ATokenIssuedByThePrivateKeyVerifiesWithTheExportedPublicHalf(t *testing.T) {
	priv, err := GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPublicKey(bytes.NewReader(PublicKey(priv)))
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}

	wantTenant, wantUser := uuid.New(), uuid.New()
	tok, err := NewDevIssuer(priv).Issue(wantTenant, wantUser, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gotTenant, gotUser, err := NewDevVerifier(loaded).Verify(tok)
	if err != nil || gotTenant != wantTenant || gotUser != wantUser {
		t.Fatalf("Verify with the loaded public key = %s, %s, %v", gotTenant, gotUser, err)
	}
}

func TestLoadPublicKey_RefusesAnyOtherLength(t *testing.T) {
	priv, err := GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"empty":                 {},
		"short":                 PublicKey(priv)[:31],
		"one byte over":         append(append([]byte{}, PublicKey(priv)...), 0),
		"the private key (64B)": priv,
	} {
		if _, err := LoadPublicKey(bytes.NewReader(content)); err == nil {
			t.Errorf("%s: LoadPublicKey accepted %d bytes", name, len(content))
		}
	}
}
