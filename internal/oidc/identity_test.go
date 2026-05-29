package oidc

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genSSHKeyPair returns an authorized_keys-format public key line for testing.
func genSSHKeyPair(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return line
}

// silenceUnused keeps pem imported for future cert-based tests.
var _ = pem.Block{}

func TestFingerprintDeterministic(t *testing.T) {
	line := genSSHKeyPair(t, "deterministic@host")

	fp1, err := Fingerprint(line)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fp2, err := Fingerprint(line)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprints differ: %s vs %s", fp1, fp2)
	}
	if !strings.HasPrefix(fp1, "SHA256:") {
		t.Fatalf("expected SHA256: prefix, got %s", fp1)
	}
}

func TestFingerprintDistinct(t *testing.T) {
	a := genSSHKeyPair(t, "a@host")
	b := genSSHKeyPair(t, "b@host")

	fpA, _ := Fingerprint(a)
	fpB, _ := Fingerprint(b)
	if fpA == fpB {
		t.Fatalf("expected distinct fingerprints, both %s", fpA)
	}
}

func TestStoreAddListRemove(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	line := genSSHKeyPair(t, "alice@laptop")
	ident, err := store.Add(line)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ident.Fingerprint == "" {
		t.Fatal("fingerprint empty")
	}
	if ident.Comment != "alice@laptop" {
		t.Fatalf("comment: %q", ident.Comment)
	}

	got := store.List()
	if len(got) != 1 || got[0].Fingerprint != ident.Fingerprint {
		t.Fatalf("List: %+v", got)
	}

	// Persisted to disk
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file on disk, got %d", len(entries))
	}

	// Lookup
	if _, ok := store.Lookup(ident.Fingerprint); !ok {
		t.Fatal("Lookup miss")
	}

	// Remove
	removed, err := store.Remove(ident.Fingerprint)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true")
	}
	if store.Count() != 0 {
		t.Fatalf("store should be empty, has %d", store.Count())
	}
}

func TestStoreLoadFromDir(t *testing.T) {
	dir := t.TempDir()

	// Drop two pub files directly on disk.
	for i, comment := range []string{"one@h", "two@h"} {
		line := genSSHKeyPair(t, comment)
		path := filepath.Join(dir, "key"+string(rune('a'+i))+".pub")
		if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Also a non-.pub file that should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	store := NewStore(dir)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if store.Count() != 2 {
		t.Fatalf("expected 2 identities, got %d", store.Count())
	}
}

func TestStoreLoadMissingDirOK(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := store.Load(); err != nil {
		t.Fatalf("Load should ignore missing dir, got: %v", err)
	}
	if store.Count() != 0 {
		t.Fatal("expected empty store")
	}
}

func TestStoreAddFromFile(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	line := genSSHKeyPair(t, "from-file@h")
	src := filepath.Join(srcDir, "id.pub")
	if err := os.WriteFile(src, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	store := NewStore(dir)
	ident, err := store.AddFromFile(src)
	if err != nil {
		t.Fatalf("AddFromFile: %v", err)
	}
	if ident.Comment != "from-file@h" {
		t.Fatalf("comment: %q", ident.Comment)
	}
}
