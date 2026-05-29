// Package oidc implements a local OIDC provider that uses SSH public keys
// as identities. The provider issues JWTs whose `sub` claim is the SHA-256
// fingerprint of an SSH public key registered with the provider.
//
// This is the MVP scope: discovery, JWKS, and a CLI token endpoint. Browser-based
// authorization code + PKCE flows are deferred to a follow-up PR (see issue #43).
package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Identity represents a registered SSH-key-as-identity entry.
type Identity struct {
	// Fingerprint is the SHA-256 fingerprint of the SSH public key.
	// Format: "SHA256:<base64-no-padding>".
	Fingerprint string
	// Comment is the trailing comment on the SSH public key (often a user@host label).
	Comment string
	// PublicKey is the raw OpenSSH-format public key string (single line).
	PublicKey string
	// Path is the on-disk path of the .pub file, if loaded from disk.
	Path string
}

// Store is a thread-safe registry of SSH-key identities backed by a directory of .pub files.
type Store struct {
	mu  sync.RWMutex
	dir string
	// byFingerprint indexes identities by SHA-256 fingerprint for O(1) lookup.
	byFingerprint map[string]Identity
}

// NewStore returns an empty Store rooted at dir. Call Load to populate it from disk.
func NewStore(dir string) *Store {
	return &Store{
		dir:           dir,
		byFingerprint: make(map[string]Identity),
	}
}

// Dir returns the directory where identity .pub files are stored.
func (s *Store) Dir() string { return s.dir }

// Load reads all *.pub files from the store directory and replaces the in-memory index.
// Missing directories are not an error; the store is simply left empty.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.byFingerprint = make(map[string]Identity)

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read identity dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		ident, err := loadIdentityFile(path)
		if err != nil {
			// Skip malformed files but do not abort the whole load.
			continue
		}
		s.byFingerprint[ident.Fingerprint] = ident
	}
	return nil
}

// Add registers an SSH public key. If the key is already present (same fingerprint)
// the existing record is replaced. The key is also persisted to <dir>/<fingerprint-fragment>.pub.
func (s *Store) Add(pubKeyAuthorizedLine string) (Identity, error) {
	ident, err := identityFromAuthorizedKey(pubKeyAuthorizedLine)
	if err != nil {
		return Identity{}, err
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Identity{}, fmt.Errorf("create identity dir: %w", err)
	}

	path := filepath.Join(s.dir, fingerprintToFilename(ident.Fingerprint))
	ident.Path = path
	if err := os.WriteFile(path, []byte(strings.TrimSpace(ident.PublicKey)+"\n"), 0o644); err != nil {
		return Identity{}, fmt.Errorf("write identity file: %w", err)
	}

	s.mu.Lock()
	s.byFingerprint[ident.Fingerprint] = ident
	s.mu.Unlock()
	return ident, nil
}

// AddFromFile reads a .pub file and registers its key.
func (s *Store) AddFromFile(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("read pubkey file: %w", err)
	}
	return s.Add(string(data))
}

// Remove deletes an identity by fingerprint. Returns true if an identity was removed.
func (s *Store) Remove(fingerprint string) (bool, error) {
	s.mu.Lock()
	ident, ok := s.byFingerprint[fingerprint]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}
	delete(s.byFingerprint, fingerprint)
	s.mu.Unlock()

	if ident.Path != "" {
		if err := os.Remove(ident.Path); err != nil && !os.IsNotExist(err) {
			return true, fmt.Errorf("remove identity file: %w", err)
		}
	}
	return true, nil
}

// Lookup returns an identity by fingerprint, if registered.
func (s *Store) Lookup(fingerprint string) (Identity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ident, ok := s.byFingerprint[fingerprint]
	return ident, ok
}

// List returns all registered identities, sorted by fingerprint.
func (s *Store) List() []Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Identity, 0, len(s.byFingerprint))
	for _, ident := range s.byFingerprint {
		out = append(out, ident)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// Count returns the number of registered identities.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byFingerprint)
}

// Fingerprint computes the SHA-256 fingerprint of an OpenSSH authorized_keys line
// (e.g. "ssh-ed25519 AAAA... user@host"). The result is in the standard
// "SHA256:<base64-no-padding>" format that OpenSSH itself prints.
func Fingerprint(authorizedKeyLine string) (string, error) {
	pub, _, err := parseAuthorizedKey(authorizedKeyLine)
	if err != nil {
		return "", err
	}
	return FingerprintPublicKey(pub), nil
}

// parseAuthorizedKey wraps ssh.ParseAuthorizedKey to avoid dogsled-style ignored returns at call sites.
func parseAuthorizedKey(line string) (ssh.PublicKey, string, error) {
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, "", fmt.Errorf("parse authorized key: %w", err)
	}
	return pub, comment, nil
}

// FingerprintPublicKey computes the SHA-256 fingerprint of an already-parsed ssh.PublicKey.
func FingerprintPublicKey(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// identityFromAuthorizedKey parses a single authorized_keys line into an Identity.
func identityFromAuthorizedKey(line string) (Identity, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return Identity{}, errors.New("empty public key")
	}
	pub, comment, err := parseAuthorizedKey(trimmed)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Fingerprint: FingerprintPublicKey(pub),
		Comment:     comment,
		PublicKey:   trimmed,
	}, nil
}

// loadIdentityFile reads a .pub file from disk and returns its Identity.
func loadIdentityFile(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	ident, err := identityFromAuthorizedKey(string(data))
	if err != nil {
		return Identity{}, err
	}
	ident.Path = path
	return ident, nil
}

// fingerprintToFilename converts a "SHA256:abcd..." fingerprint into a filesystem-safe
// filename. We replace the colon (which is fine on POSIX but ugly) and slashes that may
// appear in standard base64; RawStdEncoding does not produce padding but may produce '/'.
func fingerprintToFilename(fp string) string {
	safe := strings.ReplaceAll(fp, ":", "_")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, "+", "_")
	return safe + ".pub"
}

// DefaultIdentityDir returns the directory where bladerunner stores registered identities.
// It mirrors the XDG-compliant layout used by internal/ssh.
func DefaultIdentityDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bladerunner", "identities")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "bladerunner", "identities")
	}
	return filepath.Join(home, ".config", "bladerunner", "identities")
}
