// Package ssh provides SSH key management with XDG-compliant storage.
package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
	"golang.org/x/crypto/ssh"
)

const (
	keyFileName    = "id_ed25519"
	pubKeyFileName = "id_ed25519.pub"
	// keyLockFileName is the flock claim that serializes key generation across
	// every process on the host. The leading dot keeps it out of the way of the
	// config.d Include glob, which lives one directory down but is globbed with
	// "*" and would otherwise be a trap for any future lock file placed there.
	keyLockFileName = ".keys.lock"
	// keyComment is the trailing comment on the published public key line and
	// inside the private key. It identifies the key in an authorized_keys file.
	keyComment        = "bladerunner"
	privateKeyComment = "bladerunner VM access key"
	// pubKeyPerm is the one file in the ssh tree that is deliberately
	// world-readable: a public key is meant to be copied out. Every other file
	// here uses dirPerm/filePerm from config.go.
	pubKeyPerm os.FileMode = 0o644
)

// KeyPair holds paths and content for an SSH key pair.
type KeyPair struct {
	PrivateKeyPath string
	PublicKeyPath  string
	PublicKey      string // OpenSSH format public key string
}

// EnsureKeyPair ensures an ed25519 SSH key pair exists at the XDG-compliant
// bladerunner config location. If keys don't exist, they are generated.
//
// Keys are stored in: $XDG_CONFIG_HOME/bladerunner/ssh/ (default: ~/.config/bladerunner/ssh/)
//
// # Why there is no two-file transaction
//
// A keypair is two files, and no filesystem offers an atomic rename of two paths
// at once, so "publish both or neither" is not available: even two
// util.WriteFileAtomic calls are two steps, and a crash between them leaves one
// generation's private key beside another's public key. That is precisely the
// state that produces "Permission denied (publickey)" — a failure that reads
// like a provisioning bug and is not one.
//
// The transaction is therefore not built; it is made unnecessary. The two halves
// are NOT independent: the public key is a pure function of the private key. So
// this function treats the private key as the ONLY source of truth and derives
// the public key from it on every call, rather than trusting the .pub file. The
// .pub file becomes a cache — written for the benefit of ssh and of the user,
// never read as authority. A stale or foreign .pub cannot mislead a caller,
// because nothing downstream of here ever reads it; it is instead repaired from
// the private key. The value returned in KeyPair.PublicKey, which is what
// reaches cloud-init, is always the one that belongs to the private key on disk.
//
// That leaves one genuine ordering rule, which the code below follows: write the
// private key FIRST. If publishing the .pub then fails, the identity is intact
// and the next call repairs the cache. The reverse order would publish a public
// key whose private half was never stored.
//
// Concurrency is handled separately, by an flock held across the whole
// read-decide-write sequence, so two first starts cannot both conclude that no
// key exists. The lock is what makes the operation atomic against other
// PROCESSES; deriving the public key is what makes it honest against a crash,
// which no lock can cover.
func EnsureKeyPair() (*KeyPair, error) {
	sshDir := Dir()
	if err := os.MkdirAll(sshDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create ssh directory: %w", err)
	}

	lock, err := acquireLock(filepath.Join(sshDir, keyLockFileName))
	if err != nil {
		return nil, err
	}
	defer lock.release()

	privPath := filepath.Join(sshDir, keyFileName)
	pubPath := filepath.Join(sshDir, pubKeyFileName)

	pubKeyStr, err := loadOrGenerate(privPath)
	if err != nil {
		return nil, err
	}
	if err := ensurePublicKeyFile(pubPath, pubKeyStr); err != nil {
		return nil, err
	}
	return &KeyPair{
		PrivateKeyPath: privPath,
		PublicKeyPath:  pubPath,
		PublicKey:      pubKeyStr,
	}, nil
}

// loadOrGenerate returns the OpenSSH public key line for the private key at
// privPath, generating a new key pair if no private key is there yet. The caller
// must hold the key lock.
//
// A private key that exists but cannot be parsed is reported, NOT replaced.
// Overwriting it would destroy the only copy of an identity every already
// provisioned guest trusts, and an unreadable file is more often a permissions
// problem or a half-restored backup than a key worth discarding. AGENTS.md
// section 8 puts that decision with the user.
func loadOrGenerate(privPath string) (string, error) {
	data, err := os.ReadFile(privPath)
	switch {
	case os.IsNotExist(err):
		return generateKeyPair(privPath)
	case err != nil:
		return "", fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return "", fmt.Errorf("parse private key %s (move it aside to generate a new identity): %w", privPath, err)
	}
	return authorizedKeyLine(signer.PublicKey()), nil
}

// generateKeyPair creates a new ed25519 identity and publishes the private half
// atomically, returning the public key line derived from it. The caller must
// hold the key lock.
func generateKeyPair(privPath string) (string, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	privPEM, err := ssh.MarshalPrivateKey(privKey, privateKeyComment)
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPubKey, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", fmt.Errorf("create ssh public key: %w", err)
	}
	// The private key goes down first and in one rename: a reader either sees no
	// key at all or sees a complete one, never a half-written PEM block.
	if err := util.WriteFileAtomic(privPath, pem.EncodeToMemory(privPEM), filePerm); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}
	return authorizedKeyLine(sshPubKey), nil
}

// ensurePublicKeyFile makes the .pub cache agree with want, rewriting it if the
// contents or the mode have drifted. The caller must hold the key lock.
func ensurePublicKeyFile(pubPath, want string) error {
	if publicKeyFileMatches(pubPath, want) {
		return nil
	}
	if err := util.WriteFileAtomic(pubPath, []byte(want+"\n"), pubKeyPerm); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}

// publicKeyFileMatches reports whether the .pub file already holds want at the
// expected mode. A missing, stale, foreign or wrongly-moded file is a miss.
func publicKeyFileMatches(pubPath, want string) bool {
	info, err := os.Stat(pubPath)
	if err != nil || info.Mode().Perm() != pubKeyPerm {
		return false
	}
	data, err := os.ReadFile(pubPath)
	return err == nil && strings.TrimSpace(string(data)) == want
}

// authorizedKeyLine renders a public key the way an authorized_keys file and the
// .pub cache both spell it, with the bladerunner comment appended.
func authorizedKeyLine(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + keyComment
}

// Dir returns the XDG-compliant SSH directory for bladerunner.
// Precedence: XDG_CONFIG_HOME/bladerunner/ssh > ~/.config/bladerunner/ssh
func Dir() string {
	return filepath.Join(ConfigDir(), "ssh")
}

// ConfigDir returns the XDG-compliant config directory for bladerunner.
// internal/config owns the XDG lookup; this wraps it.
func ConfigDir() string {
	return config.DefaultConfigDir()
}
