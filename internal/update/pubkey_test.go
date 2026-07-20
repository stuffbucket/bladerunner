package update

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// prodKeyIDDisplay is the minisign-displayed key id of the embedded production
// public key. Minisign renders the 8 key-id bytes little-endian, so the parsed
// keyID (big-endian storage order) reversed must equal this. Pinning it here
// guards against a fat-fingered or truncated key silently replacing the real
// one (which would be a fail-open regression).
const prodKeyIDDisplay = "BF65715BAE1C1F9F"

// displayKeyID renders a parsed 8-byte key id the way minisign prints it:
// little-endian (reverse the stored byte order), uppercase hex.
func displayKeyID(id [8]byte) string {
	reversed := make([]byte, len(id))
	for i, b := range id {
		reversed[len(id)-1-i] = b
	}
	return strings.ToUpper(hex.EncodeToString(reversed))
}

// TestProductionPublicKey_Embedded proves the embedded key is a real, correctly
// wired minisign public key: it parses through the production path, yields a
// valid 32-byte Ed25519 key, and carries the expected key id. If any of these
// break, self-update must refuse to run rather than trust the wrong key.
func TestProductionPublicKey_Embedded(t *testing.T) {
	if productionPublicKey == "" {
		t.Fatal("productionPublicKey is empty: the production key must be embedded")
	}

	pk, err := parsePublicKey(productionPublicKey)
	if err != nil {
		t.Fatalf("parse embedded production public key: %v", err)
	}

	if len(pk.key) != ed25519.PublicKeySize {
		t.Fatalf("embedded key wrong length: got %d want %d", len(pk.key), ed25519.PublicKeySize)
	}

	if got := displayKeyID(pk.keyID); got != prodKeyIDDisplay {
		t.Fatalf("embedded key id = %s, want %s (truncated/wrong key?)", got, prodKeyIDDisplay)
	}
}

// TestProductionPublicKey_FailClosedWhenEmpty documents the fail-closed
// invariant: with no pinned key, verification MUST reject every artifact so a
// build that blanks the key can never install anything unverified.
func TestProductionPublicKey_FailClosedWhenEmpty(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("any artifact")
	sig := kp.sign(data, "timestamp:1\tfile:x")

	if err := verifyTarball(data, sig, ""); err == nil {
		t.Fatal("verifyTarball with empty public key returned nil (fail-open!)")
	}
}
