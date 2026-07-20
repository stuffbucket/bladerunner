package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// testKeypair is a throwaway minisign-style keypair used to build valid and
// tampered signatures entirely in-test — no real production key involved.
type testKeypair struct {
	keyID [8]byte
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newTestKeypair(t *testing.T) *testKeypair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	kp := &testKeypair{pub: pub, priv: priv}
	if _, err := rand.Read(kp.keyID[:]); err != nil {
		t.Fatalf("random key id: %v", err)
	}
	return kp
}

// pubKeyB64 returns the public key encoded the way tauri/macos-builder embeds
// it: base64 of the minisign public-key file text.
func (kp *testKeypair) pubKeyB64() string {
	record := append([]byte(sigAlgPrehashed), kp.keyID[:]...)
	record = append(record, kp.pub...)
	line := base64.StdEncoding.EncodeToString(record)
	text := "untrusted comment: minisign public key\n" + line + "\n"
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// sign builds a tauri-format .sig string over data using the prehashed (ED)
// algorithm, matching macos-builder's tauri signer.
func (kp *testKeypair) sign(data []byte, trustedComment string) string {
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(kp.priv, h[:])

	record := append([]byte(sigAlgPrehashed), kp.keyID[:]...)
	record = append(record, sig...)
	sigLine := base64.StdEncoding.EncodeToString(record)

	// Global signature covers sig || trusted-comment text.
	global := append([]byte{}, sig...)
	global = append(global, trustedComment...)
	globalSig := ed25519.Sign(kp.priv, global)
	globalLine := base64.StdEncoding.EncodeToString(globalSig)

	text := "untrusted comment: signature\n" +
		sigLine + "\n" +
		trustedCommentTag + trustedComment + "\n" +
		globalLine + "\n"
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// signLegacy builds a legacy (Ed, non-prehashed) signature: the ed25519
// signature is over the raw data.
func (kp *testKeypair) signLegacy(data []byte, trustedComment string) string {
	sig := ed25519.Sign(kp.priv, data)

	record := append([]byte(sigAlgLegacy), kp.keyID[:]...)
	record = append(record, sig...)
	sigLine := base64.StdEncoding.EncodeToString(record)

	global := append([]byte{}, sig...)
	global = append(global, trustedComment...)
	globalSig := ed25519.Sign(kp.priv, global)
	globalLine := base64.StdEncoding.EncodeToString(globalSig)

	text := "untrusted comment: signature\n" +
		sigLine + "\n" +
		trustedCommentTag + trustedComment + "\n" +
		globalLine + "\n"
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func TestVerifyTarball_Valid(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("this is a fake Bladerunner.app.tar.gz payload")

	tests := []struct {
		name string
		sig  string
	}{
		{"prehashed", kp.sign(data, "timestamp:123\tfile:Bladerunner.app.tar.gz")},
		{"legacy", kp.signLegacy(data, "timestamp:123\tfile:Bladerunner.app.tar.gz")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyTarball(data, tc.sig, kp.pubKeyB64()); err != nil {
				t.Fatalf("verifyTarball on valid artifact: %v", err)
			}
		})
	}
}

func TestVerifyTarball_TamperedData(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("original payload")
	sig := kp.sign(data, "timestamp:1\tfile:x")

	tampered := []byte("original payload!") // one byte appended
	err := verifyTarball(tampered, sig, kp.pubKeyB64())
	if err == nil {
		t.Fatal("expected verify to REJECT tampered data, got nil")
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected signature verification failure, got: %v", err)
	}
}

func TestVerifyTarball_TamperedSignature(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("payload")
	sig := kp.sign(data, "timestamp:1\tfile:x")

	// Flip a byte inside the decoded signature record and re-encode.
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	lines := strings.Split(string(decoded), "\n")
	recBytes, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatalf("decode record: %v", err)
	}
	recBytes[20] ^= 0xff // corrupt the ed25519 signature portion
	lines[1] = base64.StdEncoding.EncodeToString(recBytes)
	tamperedSig := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))

	if err := verifyTarball(data, tamperedSig, kp.pubKeyB64()); err == nil {
		t.Fatal("expected verify to REJECT tampered signature, got nil")
	}
}

func TestVerifyTarball_WrongKey(t *testing.T) {
	signer := newTestKeypair(t)
	// A different key with the SAME key id would still fail on the ed25519
	// check; a different key id fails earlier with ErrKeyIDMismatch. Cover both.
	data := []byte("payload")
	sig := signer.sign(data, "timestamp:1\tfile:x")

	other := newTestKeypair(t)
	err := verifyTarball(data, sig, other.pubKeyB64())
	if err == nil {
		t.Fatal("expected verify to REJECT signature from an untrusted key")
	}

	// Same key id, different key material: should fail the ed25519 check.
	imposter := newTestKeypair(t)
	imposter.keyID = signer.keyID
	if err := verifyTarball(data, sig, imposter.pubKeyB64()); err == nil {
		t.Fatal("expected verify to REJECT when key id matches but key differs")
	}
}

func TestVerifyTarball_TamperedTrustedComment(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("payload")
	sig := kp.sign(data, "timestamp:1\tfile:legit")

	// Rewrite the trusted comment line without re-signing the global signature.
	decoded, _ := base64.StdEncoding.DecodeString(sig)
	lines := strings.Split(string(decoded), "\n")
	lines[2] = trustedCommentTag + "timestamp:1\tfile:evil"
	tampered := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))

	if err := verifyTarball(data, tampered, kp.pubKeyB64()); err == nil {
		t.Fatal("expected verify to REJECT a swapped trusted comment (global sig mismatch)")
	}
}

func TestVerifyTarball_MalformedInputs(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("payload")
	goodSig := kp.sign(data, "timestamp:1\tfile:x")
	goodPub := kp.pubKeyB64()

	tests := []struct {
		name    string
		data    []byte
		sig     string
		pub     string
		wantErr string
	}{
		{"empty signature", data, "", goodPub, "malformed signature"},
		{"empty pubkey", data, goodSig, "", "wrong length"},
		{"non-base64 signature", data, "!!!not base64!!!", goodPub, "decode signature"},
		{"non-base64 pubkey", data, goodSig, "@@@", "public key"},
		{"pubkey wrong length", data, goodSig, base64.StdEncoding.EncodeToString([]byte("short")), "wrong length"},
		{"signature too few lines", data, base64.StdEncoding.EncodeToString([]byte("untrusted comment: x\nAAAA\n")), goodPub, "malformed signature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyTarball(tc.data, tc.sig, tc.pub)
			if err == nil {
				t.Fatalf("expected error, got nil (fail-closed violated)")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestVerifyTarball_UnsupportedAlgorithm(t *testing.T) {
	kp := newTestKeypair(t)
	data := []byte("payload")

	// Build a signature record with a bogus 2-byte algorithm tag.
	record := append([]byte("XX"), kp.keyID[:]...)
	record = append(record, make([]byte, 64)...)
	sigLine := base64.StdEncoding.EncodeToString(record)
	text := "untrusted comment: sig\n" + sigLine + "\n" + trustedCommentTag + "x\n" +
		base64.StdEncoding.EncodeToString(make([]byte, 64)) + "\n"
	sig := base64.StdEncoding.EncodeToString([]byte(text))

	if err := verifyTarball(data, sig, kp.pubKeyB64()); err == nil {
		t.Fatal("expected rejection of unsupported algorithm")
	}
}

func TestParsePublicKey_BareBase64(t *testing.T) {
	// A bare 42-byte payload (no minisign comment lines) should also parse, for
	// operator convenience.
	kp := newTestKeypair(t)
	record := append([]byte(sigAlgPrehashed), kp.keyID[:]...)
	record = append(record, kp.pub...)
	bare := base64.StdEncoding.EncodeToString(record)

	pk, err := parsePublicKey(bare)
	if err != nil {
		t.Fatalf("parse bare public key: %v", err)
	}
	if pk.keyID != kp.keyID {
		t.Fatal("key id mismatch after parse")
	}
}
