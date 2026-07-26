package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestVolumeNameIsAlwaysACandidate ties pack time to detection time: a browsable
// cartridge is only ever noticed because its VOLUME name carries the
// bladerunner- prefix, so the name `hdiutil create -volname` bakes in must
// satisfy the IsCandidate prefilter for every legal cartridge name. If these two
// ever drift, cartridges mount and are silently never offered for boot.
func TestVolumeNameIsAlwaysACandidate(t *testing.T) {
	for _, name := range []string{"demo", "a", "smoke-cartridge", "debian-trixie-gui"} {
		vol := VolumeName(name)
		if !IsCandidate(vol) {
			t.Errorf("IsCandidate(VolumeName(%q)) = false for volume %q", name, vol)
		}
		if got := NameFromVolume(vol); got != name {
			t.Errorf("NameFromVolume(%q) = %q, want %q", vol, got, name)
		}
		// The volume macOS actually creates when the name collides must still
		// round-trip: browsable mounts make this the common case, not the edge.
		if suffixed := vol + " 1"; !IsCandidate(suffixed) || NameFromVolume(suffixed) != name {
			t.Errorf("collision-suffixed volume %q did not round-trip", suffixed)
		}
	}
	// createArgs is the single place the volume name is written; assert it uses
	// VolumeName rather than a hand-rolled prefix.
	args := createArgs("/tmp/demo"+SparseExt, "demo", MinSizeGiB)
	found := ""
	for i, a := range args {
		if a == "-volname" && i+1 < len(args) {
			found = args[i+1]
		}
	}
	if found != VolumeName("demo") {
		t.Fatalf("createArgs -volname = %q, want %q", found, VolumeName("demo"))
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

// TestAttachArgs pins the private policy's argument vector byte-for-byte: it is
// the behavior every scripted and headless caller has always depended on, and
// the browsable inversion must not have disturbed it.
func TestAttachArgs(t *testing.T) {
	got := attachArgs(attachRequest{path: "/tmp/foo.sparseimage", mountpoint: "/mnt/foo", policy: MountPrivate})
	want := []string{
		"attach", "/tmp/foo.sparseimage",
		"-mountpoint", "/mnt/foo",
		"-nobrowse",
		"-owners", "on",
		"-noverify",
		"-plist",
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

// --- hdiutil attach -plist parsing ---------------------------------------

// sampleAttachPlist is REAL, verbatim output captured from
//
//	hdiutil create -type SPARSE -fs APFS -volname bladerunner-sample -size 1g \
//	    -nospotlight -quiet /tmp/brcart.5kKPp5/sample
//	hdiutil attach /tmp/brcart.5kKPp5/sample.sparseimage \
//	    -mountpoint /tmp/brcart.5kKPp5/mnt -nobrowse -owners on -noverify -plist
//
// on macOS 15 (Darwin 25.5.0). It exercises everything the parser must survive:
// four system-entities, three of which have NO mount-point (the GUID partition
// scheme, the Apple_APFS container, and the synthesized APFS volume group),
// exactly one mounted volume, <true/>/<false/> booleans, a DOCTYPE referencing
// Apple's external DTD, and a dev node on a DIFFERENT disk (disk9) from the
// image's own whole-disk device (disk6) — so "first entity wins" would be wrong.
const sampleAttachPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict>
			<key>content-hint</key>
			<string>GUID_partition_scheme</string>
			<key>dev-entry</key>
			<string>/dev/disk6</string>
			<key>potentially-mountable</key>
			<false/>
			<key>unmapped-content-hint</key>
			<string>GUID_partition_scheme</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>Apple_APFS</string>
			<key>dev-entry</key>
			<string>/dev/disk6s1</string>
			<key>potentially-mountable</key>
			<false/>
			<key>unmapped-content-hint</key>
			<string>7C3457EF-0000-11AA-AA11-00306543ECAC</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>41504653-0000-11AA-AA11-00306543ECAC</string>
			<key>dev-entry</key>
			<string>/dev/disk9s1</string>
			<key>mount-point</key>
			<string>/private/tmp/brcart.5kKPp5/mnt</string>
			<key>potentially-mountable</key>
			<true/>
			<key>unmapped-content-hint</key>
			<string>41504653-0000-11AA-AA11-00306543ECAC</string>
			<key>volume-kind</key>
			<string>apfs</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>EF57347C-0000-11AA-AA11-00306543ECAC</string>
			<key>dev-entry</key>
			<string>/dev/disk9</string>
			<key>potentially-mountable</key>
			<false/>
			<key>unmapped-content-hint</key>
			<string>EF57347C-0000-11AA-AA11-00306543ECAC</string>
		</dict>
	</array>
</dict>
</plist>
`

// sampleMountPoint / sampleDevNode are the mounted volume in sampleAttachPlist.
const (
	sampleMountPoint = "/private/tmp/brcart.5kKPp5/mnt"
	sampleDevNode    = "/dev/disk9s1"
)

// attachPlistFor renders a minimal two-entity plist (one unmounted whole-disk
// entity, one mounted volume) for an arbitrary mountpoint, so tests that must
// match a t.TempDir() path can still parse realistic input.
func attachPlistFor(mountpoint, devNode string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict>
			<key>content-hint</key>
			<string>GUID_partition_scheme</string>
			<key>dev-entry</key>
			<string>/dev/disk42</string>
			<key>potentially-mountable</key>
			<false/>
		</dict>
		<dict>
			<key>dev-entry</key>
			<string>%s</string>
			<key>mount-point</key>
			<string>%s</string>
			<key>potentially-mountable</key>
			<true/>
			<key>volume-kind</key>
			<string>apfs</string>
		</dict>
	</array>
</dict>
</plist>
`, devNode, mountpoint)
}

func TestParseAttachEntities(t *testing.T) {
	entities, err := parseAttachEntities(sampleAttachPlist)
	if err != nil {
		t.Fatalf("parseAttachEntities: %v", err)
	}
	if len(entities) != 4 {
		t.Fatalf("entities = %d, want 4: %+v", len(entities), entities)
	}

	want := []systemEntity{
		{DevEntry: "/dev/disk6"},
		{DevEntry: "/dev/disk6s1"},
		{DevEntry: sampleDevNode, MountPoint: sampleMountPoint, VolumeKind: "apfs"},
		{DevEntry: "/dev/disk9"},
	}
	for i, w := range want {
		if entities[i] != w {
			t.Errorf("entity[%d] = %+v, want %+v", i, entities[i], w)
		}
	}
}

func TestParseAttachEntitiesRejectsNonPlist(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"plain hdiutil text", "/dev/disk9s1\tApple_APFS\t/Volumes/x\n"},
		{"plist without system-entities", `<?xml version="1.0"?><plist version="1.0"><dict><key>other</key><string>x</string></dict></plist>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAttachEntities(tc.input); err == nil {
				t.Fatal("expected an error for non-plist input")
			}
		})
	}
}

func TestSelectMountedDevNode(t *testing.T) {
	sample, err := parseAttachEntities(sampleAttachPlist)
	if err != nil {
		t.Fatalf("parseAttachEntities: %v", err)
	}

	tests := []struct {
		name       string
		entities   []systemEntity
		mountpoint string
		want       string
	}{
		{
			name:       "exact mountpoint match wins over earlier entities",
			entities:   sample,
			mountpoint: sampleMountPoint,
			want:       sampleDevNode,
		},
		{
			name:       "unresolved /tmp form still matches /private/tmp",
			entities:   sample,
			mountpoint: "/tmp/brcart.5kKPp5/mnt",
			want:       sampleDevNode,
		},
		{
			name:       "no match falls back to the only mounted entity",
			entities:   sample,
			mountpoint: "/somewhere/else",
			want:       sampleDevNode,
		},
		{
			name:       "entities with no mount-point are ignored",
			entities:   []systemEntity{{DevEntry: "/dev/disk6"}, {DevEntry: "/dev/disk6s1"}},
			mountpoint: "/mnt/x",
			want:       "",
		},
		{
			name:       "empty entity list",
			entities:   nil,
			mountpoint: "/mnt/x",
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectMountedDevNode(tc.entities, tc.mountpoint); got != tc.want {
				t.Fatalf("selectMountedDevNode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachedDevNodeNeverErrors(t *testing.T) {
	if got := attachedDevNode("not a plist at all", "/mnt/x"); got != "" {
		t.Fatalf("attachedDevNode on garbage = %q, want empty", got)
	}
	if got := attachedDevNode(sampleAttachPlist, sampleMountPoint); got != sampleDevNode {
		t.Fatalf("attachedDevNode = %q, want %q", got, sampleDevNode)
	}
}

// TestAttachCapturesDevNode drives attach() with a fake hdiutil that returns a
// realistic plist, asserting the Mount carries the BSD device node without
// which DiskArbitration cannot address the volume.
func TestAttachCapturesDevNode(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt")
	const devNode = "/dev/disk7s1"
	// hdiutil reports the symlink-resolved mountpoint, so render the plist with
	// the resolved form to mirror reality (macOS: /var/... -> /private/var/...).
	f := &fakeRunner{results: []fakeResult{{stdout: attachPlistFor(resolvePath(mp), devNode)}}}

	m, err := attach(context.Background(), f, attachRequest{
		path:       "/tmp/foo.sparseimage",
		mountpoint: mp,
		policy:     MountPrivate,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if m.DevNode != devNode {
		t.Fatalf("Mount.DevNode = %q, want %q", m.DevNode, devNode)
	}
	if m.Mountpoint != resolvePath(mp) {
		t.Fatalf("Mount.Mountpoint = %q, want %q", m.Mountpoint, resolvePath(mp))
	}
	if m.Path != "/tmp/foo.sparseimage" {
		t.Fatalf("Mount.Path = %q", m.Path)
	}
	if m.Policy != MountPrivate {
		t.Fatalf("Mount.Policy = %q, want %q", m.Policy, MountPrivate)
	}
}

// TestAttachSucceedsWithoutPlist pins the "plist is additive" contract: an
// hdiutil that printed nothing parseable must still yield a usable Mount.
func TestAttachSucceedsWithoutPlist(t *testing.T) {
	mp := filepath.Join(t.TempDir(), "mnt")
	f := &fakeRunner{results: []fakeResult{{stdout: "/dev/disk7\tGUID_partition_scheme\t\n"}}}

	m, err := attach(context.Background(), f, attachRequest{
		path:       "/tmp/foo.sparseimage",
		mountpoint: mp,
		policy:     MountPrivate,
	})
	if err != nil {
		t.Fatalf("attach must not fail on an unparseable plist: %v", err)
	}
	// The temp dir is not a real mount, so the kernel fallback is skipped and
	// DevNode stays empty rather than reporting the PARENT volume's device.
	if m.DevNode != "" {
		t.Fatalf("Mount.DevNode = %q, want empty for a non-mounted path", m.DevNode)
	}
}

// --- mount identity ------------------------------------------------------

// TestIsAttachedRejectsPlainDirectory covers the regression the st.Dev check
// alone could not: an ordinary directory (or a missing one) is never a mounted
// cartridge.
func TestIsAttachedRejectsPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	if IsAttached(dir) {
		t.Fatalf("IsAttached(%q) = true for a plain directory", dir)
	}
	if IsAttached(filepath.Join(dir, "does-not-exist")) {
		t.Fatal("IsAttached = true for a missing path")
	}
	// A regular file is not a mountpoint either.
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if IsAttached(file) {
		t.Fatal("IsAttached = true for a regular file")
	}
}

func TestIsAttachedFromRequiresDevNode(t *testing.T) {
	// An empty dev node asserts nothing and must never pass.
	if IsAttachedFrom(t.TempDir(), "") {
		t.Fatal("IsAttachedFrom with an empty dev node = true")
	}
	if IsAttachedFrom(t.TempDir(), "/dev/disk99s1") {
		t.Fatal("IsAttachedFrom on a plain directory = true")
	}
}

func TestLookupMountOnRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if _, err := LookupMount("/"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("LookupMount off darwin = %v, want ErrUnsupported", err)
		}
		return
	}
	info, err := LookupMount("/")
	if err != nil {
		t.Fatalf("LookupMount(/): %v", err)
	}
	if info.Mountpoint != "/" {
		t.Errorf("Mountpoint = %q, want /", info.Mountpoint)
	}
	if !strings.HasPrefix(info.DevNode, devNodePrefix) {
		t.Errorf("DevNode = %q, want a %s* node", info.DevNode, devNodePrefix)
	}
	dev, err := DevNodeAt("/")
	if err != nil {
		t.Fatalf("DevNodeAt(/): %v", err)
	}
	if dev != info.DevNode {
		t.Errorf("DevNodeAt = %q, want %q", dev, info.DevNode)
	}
}

// TestAttachRealImageCapturesDevNode is the end-to-end proof that the -plist
// dev node we parse is the one the kernel reports for the mounted volume, and
// that IsAttached/IsAttachedFrom flip correctly across a detach.
//
// It attaches a REAL disk image, so it is gated three ways: skipped in -short
// mode, skipped off darwin, and (matching the sibling hdiutil integration test)
// opt-in via BLADERUNNER_CARTRIDGE_IT=1 so `make test` never mounts anything on
// a developer's machine by surprise.
func TestAttachRealImageCapturesDevNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-image attach in -short mode")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("cartridge images require macOS")
	}
	if os.Getenv("BLADERUNNER_CARTRIDGE_IT") != "1" {
		t.Skip("set BLADERUNNER_CARTRIDGE_IT=1 to run the hdiutil integration test")
	}
	if _, err := exec.LookPath(hdiutil); err != nil {
		t.Skip("hdiutil not found in PATH")
	}

	dir := t.TempDir()
	imgPath, err := Create(filepath.Join(dir, "devnode"), "devnode", MinSizeGiB)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(imgPath) })

	mp := filepath.Join(dir, "mnt")
	m, err := Attach(imgPath, mp)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	detached := false
	t.Cleanup(func() {
		if !detached {
			_ = Detach(m.Mountpoint)
		}
	})

	if !strings.HasPrefix(m.DevNode, devNodePrefix) {
		t.Fatalf("Mount.DevNode = %q, want a %s* node", m.DevNode, devNodePrefix)
	}
	// The plist's dev-entry must agree with what the kernel says backs the mount.
	info, err := LookupMount(m.Mountpoint)
	if err != nil {
		t.Fatalf("LookupMount(%q): %v", m.Mountpoint, err)
	}
	if info.DevNode != m.DevNode {
		t.Errorf("plist dev node %q != kernel dev node %q", m.DevNode, info.DevNode)
	}
	if info.Mountpoint != m.Mountpoint {
		t.Errorf("kernel mountpoint %q != Mount.Mountpoint %q", info.Mountpoint, m.Mountpoint)
	}
	if !IsAttached(m.Mountpoint) {
		t.Error("IsAttached = false for a freshly attached image")
	}
	if !IsAttachedFrom(m.Mountpoint, m.DevNode) {
		t.Error("IsAttachedFrom = false for its own dev node")
	}
	if IsAttachedFrom(m.Mountpoint, "/dev/disk999s9") {
		t.Error("IsAttachedFrom = true for a foreign dev node")
	}

	if err := Detach(m.Mountpoint); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	detached = true
	if IsAttached(m.Mountpoint) {
		t.Error("IsAttached = true after Detach")
	}
	if IsAttachedFrom(m.Mountpoint, m.DevNode) {
		t.Error("IsAttachedFrom = true after Detach")
	}
}
