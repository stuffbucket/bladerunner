package cartridge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records the commands it receives and returns scripted results. A
// nil result list means "succeed silently"; otherwise each call pops the next
// scripted result, and the last one repeats once exhausted.
type fakeRunner struct {
	calls   [][]string
	results []fakeResult
	idx     int
}

type fakeResult struct {
	stdout string
	stderr string
	err    error
}

// forceFlag is the hdiutil flag tests look for to confirm a forced detach.
const forceFlag = "-force"

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(f.results) == 0 {
		return "", "", nil
	}
	r := f.results[f.idx]
	if f.idx < len(f.results)-1 {
		f.idx++
	}
	return r.stdout, r.stderr, r.err
}

func (f *fakeRunner) lastCall() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func argsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSizeGiB(t *testing.T) {
	tests := []struct {
		disk int
		want int
	}{
		{disk: 0, want: MinSizeGiB},
		{disk: -5, want: MinSizeGiB},
		{disk: 1, want: MinSizeGiB}, // below the floor after headroom
		{disk: 2, want: 10},         // exactly at the floor after headroom
		{disk: 20, want: 28},        // disk plus headroom
		{disk: 100, want: 108},      // disk plus headroom
	}
	for _, tc := range tests {
		if got := SizeGiB(tc.disk); got != tc.want {
			t.Errorf("SizeGiB(%d) = %d, want %d", tc.disk, got, tc.want)
		}
	}
}

func TestVolumeName(t *testing.T) {
	if got := VolumeName("demo"); got != "bladerunner-demo" {
		t.Fatalf("VolumeName = %q, want bladerunner-demo", got)
	}
}

func TestCreateArgs(t *testing.T) {
	got := createArgs("/tmp/foo.sparseimage", "foo", 28)
	want := []string{
		"create",
		"-type", "SPARSE",
		"-fs", "APFS",
		"-volname", "bladerunner-foo",
		"-size", "28g",
		"-nospotlight",
		"-quiet",
		"/tmp/foo.sparseimage",
	}
	if !argsEqual(got, want) {
		t.Fatalf("createArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestAttachArgs(t *testing.T) {
	got := attachArgs("/tmp/foo.sparseimage", "/mnt/foo")
	want := []string{
		"attach", "/tmp/foo.sparseimage",
		"-mountpoint", "/mnt/foo",
		"-nobrowse",
		"-owners", "on",
		"-noverify",
	}
	if !argsEqual(got, want) {
		t.Fatalf("attachArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestDetachArgs(t *testing.T) {
	if got := detachArgs("/mnt/foo", false); !argsEqual(got, []string{"detach", "/mnt/foo"}) {
		t.Fatalf("detachArgs(force=false) = %v", got)
	}
	if got := detachArgs("/mnt/foo", true); !argsEqual(got, []string{"detach", "/mnt/foo", forceFlag}) {
		t.Fatalf("detachArgs(force=true) = %v", got)
	}
}

func TestConvertArgs(t *testing.T) {
	got := convertArgs("/tmp/foo.sparseimage", formatUDZO, "/tmp/foo")
	want := []string{"convert", "/tmp/foo.sparseimage", "-format", "UDZO", "-o", "/tmp/foo", "-quiet"}
	if !argsEqual(got, want) {
		t.Fatalf("convertArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestCompactArgs(t *testing.T) {
	got := compactArgs("/tmp/foo.sparseimage")
	want := []string{"compact", "/tmp/foo.sparseimage", "-quiet"}
	if !argsEqual(got, want) {
		t.Fatalf("compactArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestCreateReturnsResolvedPath(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stdout: "created: /tmp/foo.sparseimage\n"}}}
	got, err := create(context.Background(), f, "/tmp/foo", "foo", 28)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got != "/tmp/foo.sparseimage" {
		t.Fatalf("create path = %q, want /tmp/foo.sparseimage", got)
	}
	// hdiutil was invoked with the create verb.
	if call := f.lastCall(); len(call) == 0 || call[0] != hdiutil || call[1] != "create" {
		t.Fatalf("unexpected create call: %v", call)
	}
}

func TestCreateFallsBackToExtensionedPath(t *testing.T) {
	// No "created:" line in stdout -> fall back to requested+ext.
	f := &fakeRunner{results: []fakeResult{{stdout: ""}}}
	got, err := create(context.Background(), f, "/tmp/foo", "foo", 10)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got != "/tmp/foo"+SparseExt {
		t.Fatalf("fallback path = %q, want %q", got, "/tmp/foo"+SparseExt)
	}
}

func TestCreateNoDoubleExtension(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stdout: ""}}}
	got, err := create(context.Background(), f, "/tmp/foo.sparseimage", "foo", 10)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got != "/tmp/foo.sparseimage" {
		t.Fatalf("path = %q, want no double extension", got)
	}
}

func TestCreateWrapsError(t *testing.T) {
	wantErr := errors.New("exit status 1")
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: create failed - some reason", err: wantErr}}}
	_, err := create(context.Background(), f, "/tmp/foo", "foo", 10)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error does not wrap exec err: %v", err)
	}
	if !strings.Contains(err.Error(), "some reason") {
		t.Fatalf("error missing stderr context: %v", err)
	}
}

func TestConvertToDMGResolvesPath(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stdout: "created: /tmp/ship.dmg\n"}}}
	got, err := convertToDMG(context.Background(), f, "/tmp/foo.sparseimage", "/tmp/ship")
	if err != nil {
		t.Fatalf("convertToDMG: %v", err)
	}
	if got != "/tmp/ship.dmg" {
		t.Fatalf("dmg path = %q", got)
	}
	call := f.lastCall()
	if !argsEqual(call, []string{hdiutil, "convert", "/tmp/foo.sparseimage", "-format", "UDZO", "-o", "/tmp/ship", "-quiet"}) {
		t.Fatalf("unexpected convert call: %v", call)
	}
}

func TestConvertToSparseUsesUDSP(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stdout: "created: /tmp/work.sparseimage\n"}}}
	got, err := convertToSparse(context.Background(), f, "/tmp/ship.dmg", "/tmp/work")
	if err != nil {
		t.Fatalf("convertToSparse: %v", err)
	}
	if got != "/tmp/work.sparseimage" {
		t.Fatalf("sparse path = %q", got)
	}
	call := f.lastCall()
	if call[4] != formatUDSP {
		t.Fatalf("convert format = %q, want UDSP; call=%v", call[4], call)
	}
}

// TestDetachBusyRetriesThenForces is the central busy->force behavior: a couple
// of "Resource busy" failures, then the plain detach keeps failing, so the code
// must escalate to `detach -force`.
func TestDetachBusyRetriesThenForces(t *testing.T) {
	busy := fakeResult{stderr: `hdiutil: couldn't unmount "disk5" - Resource busy`, err: errors.New("exit status 16")}
	ok := fakeResult{stdout: `"disk4" ejected.`}
	// Stay busy for every plain attempt (detachRetries+1), then the final
	// force attempt succeeds. The fake repeats its last result, so the trailing
	// ok entry serves the force call. backoff=0 keeps the test instant.
	results := make([]fakeResult, 0, detachRetries+2)
	for i := 0; i <= detachRetries; i++ {
		results = append(results, busy)
	}
	results = append(results, ok)
	f := &fakeRunner{results: results}

	if err := detachWithBackoff(context.Background(), f, "/mnt/foo", 0); err != nil {
		t.Fatalf("detach should succeed via force, got: %v", err)
	}

	// Expect detachRetries+1 plain attempts, then one force attempt.
	wantPlain := detachRetries + 1
	plain, force := 0, 0
	for _, c := range f.calls {
		if len(c) >= 2 && c[1] == "detach" {
			if c[len(c)-1] == forceFlag {
				force++
			} else {
				plain++
			}
		}
	}
	if plain != wantPlain {
		t.Errorf("plain detach attempts = %d, want %d", plain, wantPlain)
	}
	if force != 1 {
		t.Errorf("force detach attempts = %d, want 1", force)
	}
}

func TestDetachSucceedsFirstTry(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stdout: `"disk4" ejected.`}}}
	if err := detachWithBackoff(context.Background(), f, "/mnt/foo", 0); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected exactly one detach call, got %d", len(f.calls))
	}
}

func TestDetachRecoversAfterBusy(t *testing.T) {
	// Busy once, then the plain retry succeeds: no force should be issued.
	f := &fakeRunner{results: []fakeResult{
		{stderr: "Resource busy", err: errors.New("exit status 16")},
		{stdout: `"disk4" ejected.`},
	}}
	if err := detachWithBackoff(context.Background(), f, "/mnt/foo", 0); err != nil {
		t.Fatalf("detach: %v", err)
	}
	for _, c := range f.calls {
		if c[len(c)-1] == forceFlag {
			t.Fatalf("force should not be used after a successful retry: %v", f.calls)
		}
	}
}

func TestDetachAlreadyDetachedIsNoOp(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{
		{stderr: "hdiutil: detach failed - No such file or directory", err: errors.New("exit status 1")},
	}}
	if err := detachWithBackoff(context.Background(), f, "/mnt/gone", 0); err != nil {
		t.Fatalf("already-detached should be a no-op, got: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected a single attempt for already-detached, got %d", len(f.calls))
	}
}

func TestDetachNonBusyErrorDoesNotForce(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{
		{stderr: "hdiutil: detach failed - some unexpected error", err: errors.New("exit status 1")},
	}}
	err := detachWithBackoff(context.Background(), f, "/mnt/foo", 0)
	if err == nil {
		t.Fatal("expected a non-busy error to propagate")
	}
	for _, c := range f.calls {
		if c[len(c)-1] == forceFlag {
			t.Fatalf("force should not be attempted for a non-busy error: %v", f.calls)
		}
	}
}

func TestIsBusy(t *testing.T) {
	for _, s := range []string{
		`hdiutil: couldn't unmount "disk5" - Resource busy`,
		"RESOURCE BUSY",
		"couldn't unmount disk7",
	} {
		if !isBusy(s) {
			t.Errorf("isBusy(%q) = false, want true", s)
		}
	}
	if isBusy("some other error") {
		t.Error("isBusy false positive")
	}
}

func TestIsAlreadyDetached(t *testing.T) {
	for _, s := range []string{
		"No such file or directory",
		"hdiutil: no such device",
		"is not currently mounted",
	} {
		if !isAlreadyDetached(s) {
			t.Errorf("isAlreadyDetached(%q) = false, want true", s)
		}
	}
	if isAlreadyDetached("Resource busy") {
		t.Error("isAlreadyDetached false positive")
	}
}

func TestResolveOutputPath(t *testing.T) {
	tests := []struct {
		name      string
		stdout    string
		requested string
		wantExt   string
		want      string
	}{
		{"from created line", "created: /tmp/a.sparseimage\n", "/tmp/a", SparseExt, "/tmp/a.sparseimage"},
		{"case insensitive", "Created: /tmp/b.dmg", "/tmp/b", DMGExt, "/tmp/b.dmg"},
		{"fallback appends ext", "", "/tmp/c", SparseExt, "/tmp/c.sparseimage"},
		{"fallback no double", "", "/tmp/d.dmg", DMGExt, "/tmp/d.dmg"},
		{"ignores other lines", "Reading...\ncreated: /tmp/e.dmg\nDone.", "/tmp/e", DMGExt, "/tmp/e.dmg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOutputPath(tc.stdout, tc.requested, tc.wantExt); got != tc.want {
				t.Fatalf("resolveOutputPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWrapHdiutil(t *testing.T) {
	base := errors.New("exit status 16")
	err := wrapHdiutil("detach", base, "  Resource busy  ")
	if !errors.Is(err, base) {
		t.Fatal("wrapHdiutil must wrap the base error")
	}
	if !strings.Contains(err.Error(), "hdiutil detach") || !strings.Contains(err.Error(), "Resource busy") {
		t.Fatalf("unexpected wrapped message: %v", err)
	}
	// Empty stderr -> no trailing colon noise.
	bare := wrapHdiutil("create", base, "   ")
	if strings.Contains(bare.Error(), ": :") {
		t.Fatalf("empty stderr produced noisy message: %v", bare)
	}
}

// Sanity: the public off-darwin stubs are wired (this test asserts the package
// constant message regardless of platform).
func TestErrUnsupportedMessage(t *testing.T) {
	if !strings.Contains(ErrUnsupported.Error(), "require macOS") {
		t.Fatalf("ErrUnsupported message = %q", ErrUnsupported.Error())
	}
}
