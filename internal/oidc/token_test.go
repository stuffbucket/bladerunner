package oidc

import (
	"testing"
	"time"
)

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	dir := t.TempDir()
	key, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSigningKey: %v", err)
	}
	iss, err := NewIssuer(key, "http://127.0.0.1:18556", "bladerunner", time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	iss := newTestIssuer(t)
	ident := Identity{Fingerprint: "SHA256:abc", Comment: "user@host"}
	tok, claims, err := iss.Issue(ident, "bladerunner")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	if claims.Subject != ident.Fingerprint {
		t.Fatalf("sub=%s want=%s", claims.Subject, ident.Fingerprint)
	}

	got, err := iss.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != ident.Fingerprint {
		t.Fatalf("verified sub=%s", got.Subject)
	}
	if got.Audience != "bladerunner" {
		t.Fatalf("aud=%s", got.Audience)
	}
	if got.Comment != "user@host" {
		t.Fatalf("comment=%s", got.Comment)
	}
}

func TestVerifyRejectsWrongIssuer(t *testing.T) {
	iss := newTestIssuer(t)
	ident := Identity{Fingerprint: "SHA256:abc"}
	tok, _, err := iss.Issue(ident, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Build a verifier with a different issuer URL but the same signing key.
	other, err := NewIssuer(iss.key, "http://elsewhere:9999", "bladerunner", time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("expected issuer mismatch")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	// Negative TTL would be coerced to default; use 1 nanosecond instead.
	iss, err := NewIssuer(key, "http://127.0.0.1:18556", "bladerunner", time.Nanosecond)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, _, err := iss.Issue(Identity{Fingerprint: "SHA256:x"}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := iss.Verify(tok); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestSigningKeyPersists(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	k2, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if k1.KeyID != k2.KeyID {
		t.Fatalf("key id changed: %s -> %s", k1.KeyID, k2.KeyID)
	}
}

func TestJWKSContainsKey(t *testing.T) {
	iss := newTestIssuer(t)
	set := iss.JWKS()
	if len(set.Keys) != 1 {
		t.Fatalf("expected 1 jwk, got %d", len(set.Keys))
	}
	if set.Keys[0].KeyID != iss.key.KeyID {
		t.Fatalf("kid mismatch")
	}
	if set.Keys[0].Algorithm != string(signingAlgorithm) {
		t.Fatalf("alg=%s", set.Keys[0].Algorithm)
	}
}
