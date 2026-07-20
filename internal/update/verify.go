// Package update provides an in-place self-updater for the DMG-installed
// Bladerunner.app. It fetches an update manifest over HTTPS, verifies the
// downloaded .app tarball against a pinned Ed25519 public key (minisign
// format, as emitted by stuffbucket/macos-builder's `updater` artifact), and
// atomically swaps the installed bundle.
//
// The verifier deliberately re-implements the subset of minisign-verify that
// tauri's updater uses so a Bladerunner (native Go, no Tauri runtime) client
// can consume the exact same signed artifacts. See verify.go for the byte
// layout; the whole package fails closed — an unverified tarball is never
// installed.
package update

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Signature algorithm identifiers, matching minisign. "Ed" is the legacy
// (non-prehashed) variant; "ED" is prehashed (Blake2b-512 of the data). Both
// two-byte tags share the 0x45 ('E') first byte.
const (
	sigAlgLegacy    = "Ed"
	sigAlgPrehashed = "ED"
)

// Fixed field widths of the base64-decoded minisign records, matching
// minisign-verify exactly. A signature record is 74 bytes (2 algo + 8 key id +
// 64 signature); a public-key record is 42 bytes (2 algo + 8 key id + 32
// ed25519 public key); a global signature is 64 bytes.
const (
	sigRecordLen      = 74
	pubKeyRecordLen   = 42
	globalSigLen      = ed25519.SignatureSize // 64
	trustedCommentTag = "trusted comment: "
	// sigFileLines is the number of non-empty lines in a minisign signature
	// file: untrusted comment, signature record, trusted comment, global
	// signature.
	sigFileLines = 4
)

// ErrKeyIDMismatch is returned when the signature's key id does not match the
// pinned public key's key id. It is a distinct sentinel so callers (and tests)
// can tell a wrong-key artifact from a corrupt one.
var ErrKeyIDMismatch = errors.New("update: signature key id does not match trusted public key")

// ErrBadSignature is returned when an Ed25519 verification fails. Any parse or
// decode failure surfaces as a wrapped error; either way the update is refused.
var ErrBadSignature = errors.New("update: signature verification failed")

// publicKey is a parsed minisign public key: an Ed25519 key plus the 8-byte key
// id that binds signatures to it.
type publicKey struct {
	keyID [8]byte
	key   ed25519.PublicKey
}

// signature is a parsed minisign signature record.
type signature struct {
	prehashed      bool
	keyID          [8]byte
	sig            [64]byte
	trustedComment string // the text after "trusted comment: "
	globalSig      [64]byte
}

// parsePublicKey decodes a tauri/macos-builder public key. The embedded/config
// value is base64 of the minisign public-key file text (an "untrusted comment"
// line followed by a base64 payload line); we accept either that two-line form
// or a bare base64 payload for convenience.
func parsePublicKey(pub string) (*publicKey, error) {
	payload := strings.TrimSpace(pub)
	// The tauri/macos-builder form is base64 of the minisign public-key file
	// text ("untrusted comment: ...\n<payload>\n"). Detect that specific shape
	// and pull out the payload line. A bare base64 payload (42-byte record) is
	// also accepted directly, so we only take the text branch when the decode
	// actually yields a minisign comment header.
	if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
		if text := string(decoded); strings.HasPrefix(text, "untrusted comment:") {
			lines := splitLines(text)
			if len(lines) < 2 {
				return nil, fmt.Errorf("update: malformed public key text")
			}
			payload = lines[1]
		}
	}

	bin, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("update: decode public key: %w", err)
	}
	if len(bin) != pubKeyRecordLen {
		return nil, fmt.Errorf("update: public key wrong length: got %d want %d", len(bin), pubKeyRecordLen)
	}
	alg := string(bin[0:2])
	if alg != sigAlgLegacy && alg != sigAlgPrehashed {
		return nil, fmt.Errorf("update: unsupported public key algorithm %q", alg)
	}
	pk := &publicKey{key: make(ed25519.PublicKey, ed25519.PublicKeySize)}
	copy(pk.keyID[:], bin[2:10])
	copy(pk.key, bin[10:42])
	return pk, nil
}

// parseSignature decodes a tauri/macos-builder .sig value: base64 of the
// minisign signature file text (four lines: untrusted comment, base64 sig
// record, trusted comment, base64 global signature).
func parseSignature(sig string) (*signature, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return nil, fmt.Errorf("update: decode signature: %w", err)
	}
	lines := splitLines(string(decoded))
	if len(lines) < sigFileLines {
		return nil, fmt.Errorf("update: malformed signature: want %d lines, got %d", sigFileLines, len(lines))
	}

	bin1, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return nil, fmt.Errorf("update: decode signature record: %w", err)
	}
	if len(bin1) != sigRecordLen {
		return nil, fmt.Errorf("update: signature record wrong length: got %d want %d", len(bin1), sigRecordLen)
	}

	trusted := lines[2]
	if !strings.HasPrefix(trusted, trustedCommentTag) {
		return nil, fmt.Errorf("update: missing %q line", strings.TrimSpace(trustedCommentTag))
	}

	bin2, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		return nil, fmt.Errorf("update: decode global signature: %w", err)
	}
	if len(bin2) != globalSigLen {
		return nil, fmt.Errorf("update: global signature wrong length: got %d want %d", len(bin2), globalSigLen)
	}

	alg := string(bin1[0:2])
	var prehashed bool
	switch alg {
	case sigAlgLegacy:
		prehashed = false
	case sigAlgPrehashed:
		prehashed = true
	default:
		return nil, fmt.Errorf("update: unsupported signature algorithm %q", alg)
	}

	s := &signature{
		prehashed:      prehashed,
		trustedComment: strings.TrimPrefix(trusted, trustedCommentTag),
	}
	copy(s.keyID[:], bin1[2:10])
	copy(s.sig[:], bin1[10:74])
	copy(s.globalSig[:], bin2)
	return s, nil
}

// verify checks that sig is a valid minisign signature over data produced by
// the holder of pk. It mirrors minisign-verify: match the key id, prehash the
// data with Blake2b-512 when the signature is prehashed, verify the Ed25519
// signature over that, then verify the global signature over
// (signature || trusted-comment). It fails closed on the first problem.
func (pk *publicKey) verify(data []byte, sig *signature) error {
	// Constant-time key-id compare so a mismatch does not leak timing, and so a
	// caller cannot confuse "wrong key" with a bad signature.
	if subtle.ConstantTimeCompare(pk.keyID[:], sig.keyID[:]) != 1 {
		return ErrKeyIDMismatch
	}

	signed := data
	if sig.prehashed {
		h := blake2b.Sum512(data)
		signed = h[:]
	}

	if !ed25519.Verify(pk.key, signed, sig.sig[:]) {
		return ErrBadSignature
	}

	// The global signature covers the raw signature bytes concatenated with the
	// trusted-comment text, binding the comment to the artifact.
	global := make([]byte, 0, len(sig.sig)+len(sig.trustedComment))
	global = append(global, sig.sig[:]...)
	global = append(global, sig.trustedComment...)
	if !ed25519.Verify(pk.key, global, sig.globalSig[:]) {
		return ErrBadSignature
	}
	return nil
}

// verifyTarball is the package's public verification entry point: parse the
// pinned public key and the artifact's signature, then verify data against
// both. Every failure path refuses the update.
func verifyTarball(data []byte, sig, pubKey string) error {
	pk, err := parsePublicKey(pubKey)
	if err != nil {
		return err
	}
	s, err := parseSignature(sig)
	if err != nil {
		return err
	}
	return pk.verify(data, s)
}

// splitLines splits minisign text on newlines, tolerating CRLF and trailing
// blank lines, returning only non-empty lines in order.
func splitLines(s string) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
