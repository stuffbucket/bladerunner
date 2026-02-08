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

	"github.com/stuffbucket/bladerunner/internal/util"
	"golang.org/x/crypto/ssh"
)

const (
	keyFileName    = "id_ed25519"
	pubKeyFileName = "id_ed25519.pub"
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
func EnsureKeyPair() (*KeyPair, error) {
	sshDir := Dir()
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ssh directory: %w", err)
	}

	privPath := filepath.Join(sshDir, keyFileName)
	pubPath := filepath.Join(sshDir, pubKeyFileName)

	// Check if keys already exist
	if util.FileExists(privPath) && util.FileExists(pubPath) {
		pubKey, err := os.ReadFile(pubPath)
		if err != nil {
			return nil, fmt.Errorf("read public key: %w", err)
		}
		return &KeyPair{
			PrivateKeyPath: privPath,
			PublicKeyPath:  pubPath,
			PublicKey:      strings.TrimSpace(string(pubKey)),
		}, nil
	}

	// Generate new key pair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	// Encode private key in OpenSSH format
	privPEM, err := ssh.MarshalPrivateKey(privKey, "bladerunner VM access key")
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	if err := os.WriteFile(privPath, pem.EncodeToMemory(privPEM), 0o600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}

	// Encode public key in OpenSSH format
	sshPubKey, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("create ssh public key: %w", err)
	}
	pubKeyStr := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPubKey))) + " bladerunner"

	if err := os.WriteFile(pubPath, []byte(pubKeyStr+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write public key: %w", err)
	}

	return &KeyPair{
		PrivateKeyPath: privPath,
		PublicKeyPath:  pubPath,
		PublicKey:      pubKeyStr,
	}, nil
}

// Dir returns the XDG-compliant SSH directory for bladerunner.
// Precedence: XDG_CONFIG_HOME/bladerunner/ssh > ~/.config/bladerunner/ssh
func Dir() string {
	return filepath.Join(ConfigDir(), "ssh")
}

// ConfigDir returns the XDG-compliant config directory for bladerunner.
func ConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bladerunner")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "bladerunner")
	}
	return filepath.Join(home, ".config", "bladerunner")
}
