package cartridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hashOf is the fingerprint the write-back tests compare the user's ORIGINAL
// cartridge against. A test that only asserts "an error was returned" would
// pass just as happily over a half-written file.
func hashOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// persistFixture stages a shipped .dmg with a mounted, verified working copy —
// the state a cartridge is in the instant its guest powers off. It returns the
// opened cartridge, the .dmg path and the working-copy path.
func persistFixture(t *testing.T, persist bool) (*Opened, string, string) {
	t.Helper()
	tmp := t.TempDir()
	mp := filepath.Join(tmp, "mnt", "demo")
	openFixture(t, mp)
	dmg := filepath.Join(tmp, "demo"+DMGExt)
	work := filepath.Join(tmp, "demo"+SparseExt)
	writeFixtureFile(t, dmg, "the-shipped-cartridge")
	writeFixtureFile(t, work, "what-the-guest-wrote")

	o := &Opened{
		Name:        "demo",
		SourcePath:  dmg,
		WorkingCopy: work,
		Mount:       Mount{Path: work, Mountpoint: resolvePath(mp), DevNode: openTestDevNode, Policy: MountPrivate},
		Layout:      NewLayout(mp),
		persist:     persist,
	}
	t.Cleanup(func() { o.releaseClaim() })
	return o, dmg, work
}

// convertedResult scripts an `hdiutil convert` that reports where it wrote.
func convertedResult(path string) fakeResult {
	return fakeResult{stdout: "created: " + path + "\n"}
}

// stagingRunner is a fakeRunner that also does the one thing the real hdiutil
// convert does and the shared fake does not: leave a file behind.
//
// The artifact cannot simply be staged up front, because buildCommitArtifact
// deliberately clears a staging file left by an interrupted attempt before it
// converts. Producing it AT convert time is therefore the only way to exercise
// the publish, and it is what makes "the stale one was cleared" observable: the
// bytes that land are the ones this runner wrote, not the ones staged earlier.
type stagingRunner struct {
	*fakeRunner
	t       *testing.T
	path    string
	content string
}

func (s *stagingRunner) run(ctx context.Context, name string, args ...string) (string, string, error) {
	out, errOut, err := s.fakeRunner.run(ctx, name, args...)
	if err == nil && len(args) > 0 && args[0] == cmdConvert {
		writeFixtureFile(s.t, s.path, s.content)
	}
	return out, errOut, err
}

// hdiutilVerbs lists the subcommand of every hdiutil call a fake runner saw.
func hdiutilVerbs(f *fakeRunner) []string {
	verbs := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		if len(c) > 1 {
			verbs = append(verbs, c[1])
		}
	}
	return verbs
}

// The default must stay "discard": a .dmg boot has always been a throwaway run,
// and quietly starting to rewrite the user's cartridge would be a change nobody
// asked for over a file they may have been handed by someone else.
func TestCloseWithoutPersistDiscardsTheWorkingCopy(t *testing.T) {
	o, dmg, work := persistFixture(t, false)
	before := hashOf(t, dmg)
	f := &fakeRunner{results: []fakeResult{{}}}

	if err := o.closeWith(context.Background(), f); err != nil {
		t.Fatalf("closeWith: %v", err)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working copy survived a discarding close: %v", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Errorf("the .dmg changed without --persist: %s -> %s", before, got)
	}
	if verbs := hdiutilVerbs(f); len(verbs) != 1 || verbs[0] != cmdDetach {
		t.Errorf("hdiutil verbs = %v, want a detach and nothing else", verbs)
	}
}

// The happy path: detach, compact, convert, verify, then one rename over the
// original.
func TestCloseWithPersistCommitsOverTheOriginal(t *testing.T) {
	o, dmg, work := persistFixture(t, true)
	before := hashOf(t, dmg)
	commit := commitStem(dmg) + DMGExt
	base := &fakeRunner{results: []fakeResult{
		{},                       // detach
		{stdout: emptyInfoPlist}, // info: the working copy is detached
		{},                       // compact
		convertedResult(commit),  // convert -> the staged artifact
		{},                       // verify
	}}
	f := &stagingRunner{fakeRunner: base, t: t, path: commit, content: "the-committed-cartridge"}

	if err := o.closeWith(context.Background(), f); err != nil {
		t.Fatalf("closeWith: %v", err)
	}
	got := hashOf(t, dmg)
	if got == before {
		t.Fatalf("the .dmg was not written back: sha256 still %s", before)
	}
	if data, err := os.ReadFile(dmg); err != nil || string(data) != "the-committed-cartridge" {
		t.Fatalf("the .dmg holds %q, %v; want the committed artifact", data, err)
	}
	if _, err := os.Stat(commit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging artifact %s survived the publish: %v", commit, err)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working copy survived a committed close: %v", err)
	}
	wantVerbs := []string{cmdDetach, cmdInfo, cmdCompact, cmdConvert, cmdVerify}
	if verbs := hdiutilVerbs(base); !argsEqual(verbs, wantVerbs) {
		t.Errorf("hdiutil verbs = %v, want %v", verbs, wantVerbs)
	}
	// The committed file must still be a cartridge: re-openable, with the
	// manifest and metadata the pack put there.
	assertStillACartridge(t, o, dmg)
}

// assertStillACartridge re-opens the freshly committed cartridge and reads its
// manifest back, which is the only assertion that distinguishes "a file
// appeared" from "a bootable cartridge appeared". The volume contents are the
// fixture the fake attach reports, so what this proves under the fake runner is
// that the committed path re-opens and its manifest parses — the bytes
// themselves are the integration test's business.
func assertStillACartridge(t *testing.T, prev *Opened, dmg string) {
	t.Helper()
	// The claim is keyed on the working copy, and a committed close removed it,
	// so release this one before a second Open asks for the same lock.
	prev.releaseClaim()
	mp := filepath.Join(filepath.Dir(dmg), "mnt", "demo")
	f := &fakeRunner{results: []fakeResult{
		{}, // convert the committed .dmg into a fresh working copy
		attachResult(mp),
	}}
	reopened, err := open(context.Background(), f, dmg, privateOpen(mp))
	if err != nil {
		t.Fatalf("re-open the committed cartridge: %v", err)
	}
	t.Cleanup(func() { reopened.releaseClaim() })
	if reopened.Manifest == nil || reopened.Manifest.Name == "" {
		t.Fatalf("the committed cartridge carries no manifest: %+v", reopened.Manifest)
	}
	if reopened.Metadata.FormatVersion != FormatVersion {
		t.Errorf("committed cartridge format = %d, want %d", reopened.Metadata.FormatVersion, FormatVersion)
	}
}

// The safety proof. A convert that fails (a full disk, a corrupt working copy)
// must leave the user's cartridge bit-identical, must say so, and must not
// throw away the guest's changes on the way out.
func TestWriteBackFailureLeavesTheOriginalIntact(t *testing.T) {
	o, dmg, work := persistFixture(t, true)
	before := hashOf(t, dmg)
	f := &fakeRunner{results: []fakeResult{
		{},                       // detach
		{stdout: emptyInfoPlist}, // info: detached
		{},                       // compact
		{stderr: "hdiutil: convert failed - No space left on device", err: errors.New("exit 1")},
	}}

	err := o.closeWith(context.Background(), f)
	if !errors.Is(err, ErrWriteBackFailed) {
		t.Fatalf("closeWith = %v, want ErrWriteBackFailed", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("a failed write-back changed the original: %s -> %s", before, got)
	}
	if !strings.Contains(err.Error(), dmg) {
		t.Errorf("error %q does not name the untouched original %s", err, dmg)
	}
	// The guest's changes are kept, moved aside under a name the next boot of
	// the .dmg will not clear.
	if _, statErr := os.Stat(work); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the failed write-back left the working copy at its clearable path: %v", statErr)
	}
	rescued := rescuedFiles(t, filepath.Dir(dmg))
	if len(rescued) != 1 {
		t.Fatalf("rescue files = %v, want exactly one", rescued)
	}
	if data, readErr := os.ReadFile(rescued[0]); readErr != nil || string(data) != "what-the-guest-wrote" {
		t.Fatalf("rescued file holds %q, %v; want the guest's bytes", data, readErr)
	}
	if !strings.Contains(err.Error(), filepath.Base(rescued[0])) {
		t.Errorf("error %q does not tell the user where their changes went", err)
	}
}

// rescuedFiles lists the rescue images sitting in dir.
func rescuedFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"+rescueInfix+"*"+SparseExt))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// A write-back while the image is still attached would read a live backing
// store. That is also the "the VM is still running" case: a VM cannot run
// without its cartridge attached.
func TestWriteBackRefusesWhileTheImageIsAttached(t *testing.T) {
	o, dmg, work := persistFixture(t, true)
	before := hashOf(t, dmg)
	const stillMounted = "/Volumes/bladerunner-demo"
	f := &fakeRunner{results: []fakeResult{attachedImageResult(work, openTestDevNode, stillMounted)}}
	o.Mount = Mount{} // the detach reported success; hdiutil disagrees

	err := o.writeBack(context.Background(), f)
	if !errors.Is(err, ErrWriteBackAttached) {
		t.Fatalf("writeBack = %v, want ErrWriteBackAttached", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("a refused write-back changed the original: %s -> %s", before, got)
	}
	if !strings.Contains(err.Error(), stillMounted) {
		t.Errorf("error %q does not name where the volume still is", err)
	}
	if verbs := hdiutilVerbs(f); len(verbs) != 1 || verbs[0] != cmdInfo {
		t.Errorf("hdiutil verbs = %v, want the probe alone — no convert over a live image", verbs)
	}
}

// The same refusal from the other direction: the detach FAILED, so the Mount is
// still recorded and nothing may be read off that image.
func TestWriteBackRefusesWhenTheDetachFailed(t *testing.T) {
	o, dmg, _ := persistFixture(t, true)
	before := hashOf(t, dmg)
	f := &fakeRunner{}

	err := o.writeBack(context.Background(), f)
	if !errors.Is(err, ErrWriteBackAttached) {
		t.Fatalf("writeBack = %v, want ErrWriteBackAttached", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a recorded mount must be refused without asking hdiutil: %v", f.calls)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("a refused write-back changed the original: %s -> %s", before, got)
	}
}

// A probe that cannot be completed is treated as "still attached". Overwriting
// the user's cartridge from an image we could not confirm is quiescent is not a
// risk worth taking to save them one retry.
func TestWriteBackRefusesWhenAttachmentCannotBeConfirmed(t *testing.T) {
	o, dmg, _ := persistFixture(t, true)
	before := hashOf(t, dmg)
	o.Mount = Mount{}
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: info failed", err: errors.New("exit 1")}}}

	if err := o.writeBack(context.Background(), f); !errors.Is(err, ErrWriteBackAttached) {
		t.Fatalf("writeBack = %v, want ErrWriteBackAttached", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("an unconfirmed write-back changed the original: %s -> %s", before, got)
	}
}

// The same malformed output reaches the write-back's detach confirmation, which
// reads "nothing is attached from that file" as permission to compress it and
// rename the result over the user's cartridge. A record this build cannot read
// must be refused there too, or a live image gets compressed mid-write and
// shipped as a cartridge.
func TestWriteBackRefusesOnUnreadableInfoOutput(t *testing.T) {
	for name, plist := range malformedInfoPlists {
		t.Run(name, func(t *testing.T) {
			o, dmg, _ := persistFixture(t, true)
			before := hashOf(t, dmg)
			o.Mount = Mount{} // this process's own detach reported success
			f := &fakeRunner{results: []fakeResult{{stdout: plist}}}

			if err := o.writeBack(context.Background(), f); !errors.Is(err, ErrWriteBackAttached) {
				t.Fatalf("writeBack = %v, want ErrWriteBackAttached", err)
			}
			if got := hashOf(t, dmg); got != before {
				t.Fatalf("an unconfirmed write-back changed the original: %s -> %s", before, got)
			}
			if verbs := hdiutilVerbs(f); len(verbs) != 1 || verbs[0] != cmdInfo {
				t.Errorf("hdiutil verbs = %v, want the probe alone — nothing may be read off the image", verbs)
			}
		})
	}
}

// A cartridge sitting on a read-only volume (a mounted DMG, a locked share)
// cannot be replaced. Say so BEFORE spending half an hour compressing an
// artifact that has nowhere to go.
func TestWriteBackRefusesAReadOnlyLocation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	o, dmg, _ := persistFixture(t, true)
	before := hashOf(t, dmg)
	o.Mount = Mount{}
	dir := filepath.Dir(dmg)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	f := &fakeRunner{results: []fakeResult{{stdout: emptyInfoPlist}}}

	err := o.writeBack(context.Background(), f)
	if !errors.Is(err, ErrWriteBackReadOnly) {
		t.Fatalf("writeBack = %v, want ErrWriteBackReadOnly", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("a refused write-back changed the original: %s -> %s", before, got)
	}
	if verbs := hdiutilVerbs(f); len(verbs) != 1 || verbs[0] != cmdInfo {
		t.Errorf("hdiutil verbs = %v, want the probe alone — no convert we cannot publish", verbs)
	}
}

// A .sparseimage boot writes into the file directly, so there is no working
// copy and nothing to commit. --persist there is a no-op, not an error.
func TestWriteBackIsANoOpWithoutAWorkingCopy(t *testing.T) {
	o, _, _ := persistFixture(t, true)
	o.Mount = Mount{}
	o.WorkingCopy = ""
	f := &fakeRunner{}

	if err := o.writeBack(context.Background(), f); err != nil {
		t.Fatalf("writeBack = %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a no-op write-back ran hdiutil: %v", f.calls)
	}
	if o.WritesBack() {
		t.Error("WritesBack() is true with no working copy to write back")
	}
}

// A verify that fails means the new artifact is not a cartridge. It must be
// discarded, never published.
func TestWriteBackDiscardsAnArtifactThatFailsVerification(t *testing.T) {
	o, dmg, _ := persistFixture(t, true)
	before := hashOf(t, dmg)
	o.Mount = Mount{}
	commit := commitStem(dmg) + DMGExt
	base := &fakeRunner{results: []fakeResult{
		{stdout: emptyInfoPlist}, // info
		{},                       // compact
		convertedResult(commit),  // convert
		{stderr: "hdiutil: verify failed - checksum mismatch", err: errors.New("exit 1")},
	}}
	f := &stagingRunner{fakeRunner: base, t: t, path: commit, content: "truncated-garbage"}

	if err := o.writeBack(context.Background(), f); !errors.Is(err, ErrWriteBackFailed) {
		t.Fatalf("writeBack = %v, want ErrWriteBackFailed", err)
	}
	if got := hashOf(t, dmg); got != before {
		t.Fatalf("an unverified artifact was published: %s -> %s", before, got)
	}
	if _, err := os.Stat(commit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rejected artifact %s was left behind: %v", commit, err)
	}
	if verbs := hdiutilVerbs(base); verbs[len(verbs)-1] != cmdVerify {
		t.Errorf("hdiutil verbs = %v, want the verify to be what refused it", verbs)
	}
}

// A write-back interrupted after convert but before the rename leaves a staging
// file. hdiutil convert refuses to overwrite, so the next attempt has to clear
// it — otherwise persistence would be broken for good rather than for one run,
// and the bytes it published would be the interrupted run's, not this guest's.
func TestWriteBackClearsAStagingFileFromAnInterruptedAttempt(t *testing.T) {
	o, dmg, _ := persistFixture(t, true)
	o.Mount = Mount{}
	commit := commitStem(dmg) + DMGExt
	writeFixtureFile(t, commit, "half-written-by-a-killed-run")
	base := &fakeRunner{results: []fakeResult{
		{stdout: emptyInfoPlist}, // info
		{},                       // compact
		convertedResult(commit),  // convert
		{},                       // verify
	}}
	f := &stagingRunner{fakeRunner: base, t: t, path: commit, content: "this-guests-cartridge"}

	if err := o.writeBack(context.Background(), f); err != nil {
		t.Fatalf("writeBack: %v", err)
	}
	if data, err := os.ReadFile(dmg); err != nil || string(data) != "this-guests-cartridge" {
		t.Fatalf("the .dmg holds %q, %v; want this run's artifact, not the interrupted run's", data, err)
	}
}

// The write-back budget has to cover the convert, or Close would kill its own
// compression on every cartridge big enough to matter. A discarding close is a
// detach plus the `hdiutil info` probe that recovers a device node the attach
// plist never supplied.
func TestCloseBudgetCoversTheWriteBack(t *testing.T) {
	o, _, _ := persistFixture(t, true)
	if got := o.closeBudget(); got <= convertTimeout {
		t.Errorf("closeBudget() = %v, want more than the convert budget %v", got, convertTimeout)
	}
	o.persist = false
	if got := o.closeBudget(); got != detachTimeout+infoTimeout {
		t.Errorf("a discarding close budget = %v, want %v", got, detachTimeout+infoTimeout)
	}
}

// The staging artifact must be a hidden sibling of the cartridge: same
// directory, so the publish is a rename rather than a copy across filesystems,
// and hidden so a half-built artifact is not offered to the user as a cartridge.
func TestCommitStemIsAHiddenSibling(t *testing.T) {
	stem := commitStem("/Users/someone/Downloads/demo" + DMGExt)
	if dir := filepath.Dir(stem); dir != "/Users/someone/Downloads" {
		t.Errorf("staging dir = %q, want the cartridge's own directory", dir)
	}
	if base := filepath.Base(stem); !strings.HasPrefix(base, ".") {
		t.Errorf("staging name %q is not hidden", base)
	}
	if stem+DMGExt == "/Users/someone/Downloads/demo"+DMGExt {
		t.Error("the staging path collides with the cartridge it replaces")
	}
}
