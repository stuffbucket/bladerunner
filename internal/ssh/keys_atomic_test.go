package ssh_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	brssh "github.com/stuffbucket/bladerunner/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// keyComment is the trailing comment EnsureKeyPair puts on the public key.
const keyComment = " bladerunner"

// derivedPublicKey reads the PRIVATE key from disk and computes the public key
// that belongs to it. Every assertion about the published pair is made against
// this value rather than against the .pub file, because the .pub file is exactly
// the thing under test: comparing it to itself would prove nothing.
func derivedPublicKey(t *testing.T, privPath string) string {
	t.Helper()
	data, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read private key %s: %v", privPath, err)
	}
	signer, err := xssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("parse private key %s: %v", privPath, err)
	}
	return strings.TrimSpace(string(xssh.MarshalAuthorizedKey(signer.PublicKey()))) + keyComment
}

// foreignPublicKey returns an OpenSSH public key line for a key that has nothing
// to do with the one on disk.
func foreignPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap foreign key: %v", err)
	}
	return strings.TrimSpace(string(xssh.MarshalAuthorizedKey(sshPub))) + keyComment
}

// assertPublishedPairMatches is the invariant the whole fix exists to hold: the
// .pub file on disk belongs to the private key on disk, and the value handed to
// the caller (which is what reaches cloud-init) belongs to it too.
func assertPublishedPairMatches(t *testing.T, kp *brssh.KeyPair) {
	t.Helper()
	want := derivedPublicKey(t, kp.PrivateKeyPath)
	onDisk, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	if got := strings.TrimSpace(string(onDisk)); got != want {
		t.Errorf("published .pub does not belong to the published private key:\n .pub    = %s\n derived = %s", got, want)
	}
	if kp.PublicKey != want {
		t.Errorf("returned PublicKey does not belong to the published private key:\n returned = %s\n derived  = %s", kp.PublicKey, want)
	}
}

// TestEnsureKeyPairConcurrentPublishesMatchingPair is the regression test for
// the first-generation race: EnsureKeyPair used to test for the two files, then
// write them with two independent os.WriteFile calls. Several first starts could
// all observe no keypair, generate DIFFERENT keys, and interleave four writes,
// leaving one generation's private key beside another generation's public key.
// The host then publishes a public key it holds no private key for, and the
// guest answers every login with "Permission denied (publickey)".
//
// The barrier is a two-phase rendezvous, not a sleep: every goroutine reports
// itself scheduled on `ready` and then parks on `start`, and `start` is not
// closed until all of them have reported. No timing assumption is made anywhere,
// and the assertions are invariants that must hold on every run and every
// interleaving, so the test is meaningful under -race with any scheduler.
func TestEnsureKeyPairConcurrentPublishesMatchingPair(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const writers = 16
	ready := make(chan struct{}, writers)
	start := make(chan struct{})
	pairs := make([]*brssh.KeyPair, writers)
	errs := make([]error, writers)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			pairs[i], errs[i] = brssh.EnsureKeyPair()
		}()
	}
	for range writers {
		<-ready
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: EnsureKeyPair() error = %v", i, err)
		}
	}

	// Every caller must have been handed the one identity that got published.
	want := derivedPublicKey(t, pairs[0].PrivateKeyPath)
	for i, kp := range pairs {
		if kp.PublicKey != want {
			t.Errorf("writer %d got public key %s, want the single published key %s", i, kp.PublicKey, want)
		}
	}
	assertPublishedPairMatches(t, pairs[0])
}

// TestEnsureKeyPairRepairsMismatchedPublicKey covers the corruption the race can
// leave behind (and that a partial write can leave behind on its own): a .pub
// file that does not belong to the private key beside it. The old code trusted
// any two files that existed and handed the stale .pub straight back.
func TestEnsureKeyPairRepairsMismatchedPublicKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := brssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("EnsureKeyPair() error = %v", err)
	}
	if err := os.WriteFile(first.PublicKeyPath, []byte(foreignPublicKey(t)+"\n"), 0o644); err != nil {
		t.Fatalf("seed mismatched public key: %v", err)
	}

	repaired, err := brssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("second EnsureKeyPair() error = %v", err)
	}
	assertPublishedPairMatches(t, repaired)
}

// TestEnsureKeyPairKeepsPrivateKeyWhenPublicMissing pins the data-safety half of
// the rule. The old existence test was `private AND public`, so losing the
// derivable half threw away the half that cannot be derived: a missing .pub made
// EnsureKeyPair generate a whole new identity over a private key that was
// perfectly good, and every guest already provisioned with the old key became
// unreachable.
func TestEnsureKeyPairKeepsPrivateKeyWhenPublicMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	original, err := brssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("EnsureKeyPair() error = %v", err)
	}
	before, err := os.ReadFile(original.PrivateKeyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	if err := os.Remove(original.PublicKeyPath); err != nil {
		t.Fatalf("remove public key: %v", err)
	}

	restored, err := brssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("second EnsureKeyPair() error = %v", err)
	}
	after, err := os.ReadFile(restored.PrivateKeyPath)
	if err != nil {
		t.Fatalf("re-read private key: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("EnsureKeyPair replaced a valid private key because only the derivable half was missing")
	}
	assertPublishedPairMatches(t, restored)
}

// TestEnsureKeyPairFileModes holds the modes the acceptance criteria name. ssh
// refuses a private key any other account can read.
func TestEnsureKeyPairFileModes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	kp, err := brssh.EnsureKeyPair()
	if err != nil {
		t.Fatalf("EnsureKeyPair() error = %v", err)
	}
	for path, want := range map[string]os.FileMode{
		kp.PrivateKeyPath: 0o600,
		kp.PublicKeyPath:  0o644,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
		}
	}
}
