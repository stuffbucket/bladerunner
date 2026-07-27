package cartridge

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/diskarb"
)

// sampleInfoPlist is a real capture of `hdiutil info -plist` on macOS 15 with
// three images attached (only the unrelated third image's path is anonymized;
// every key, entity and value is as hdiutil emitted it). The three cover every
// shape the parser must survive:
//
//   - an unrelated third-party read/write .dmg with a mounted volume, so a
//     lookup must not return the first image it sees;
//   - a shipped read-only cartridge .dmg with FOUR system entities (partition
//     scheme, APFS container, volume group, volume) of which only the last
//     carries a mount-point — the shape a double-clicked cartridge produces;
//   - a .sparseimage attached with -nomount, i.e. FIVE entities and no
//     mount-point anywhere, which must be findable by device node and must
//     never be matched by a mountpoint lookup.
const sampleInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>framework</key>
	<string>683.100.3</string>
	<key>images</key>
	<array>
		<dict>
			<key>autodiskmount</key>
			<true/>
			<key>blockcount</key>
			<integer>229455</integer>
			<key>blocksize</key>
			<integer>512</integer>
			<key>diskimages2</key>
			<false/>
			<key>hdid-pid</key>
			<integer>47562</integer>
			<key>icon-path</key>
			<string>/System/Library/PrivateFrameworks/DiskImages.framework/Resources/CDiskImage.icns</string>
			<key>image-encrypted</key>
			<false/>
			<key>image-path</key>
			<string>/Users/someone/build/Other_0.0.0_aarch64.dmg</string>
			<key>image-type</key>
			<string>read/write</string>
			<key>owner-uid</key>
			<integer>501</integer>
			<key>removable</key>
			<true/>
			<key>system-entities</key>
			<array>
				<dict>
					<key>content-hint</key>
					<string>GUID_partition_scheme</string>
					<key>dev-entry</key>
					<string>/dev/disk4</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>48465300-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk4s1</string>
					<key>mount-point</key>
					<string>/Volumes/dmg.mrluVn</string>
				</dict>
			</array>
			<key>writeable</key>
			<true/>
		</dict>
		<dict>
			<key>autodiskmount</key>
			<true/>
			<key>blockcount</key>
			<integer>20546</integer>
			<key>blocksize</key>
			<integer>512</integer>
			<key>diskimages2</key>
			<false/>
			<key>hdid-pid</key>
			<integer>18610</integer>
			<key>icon-path</key>
			<string>/System/Library/PrivateFrameworks/DiskImages.framework/Resources/CDiskImage.icns</string>
			<key>image-encrypted</key>
			<false/>
			<key>image-path</key>
			<string>/private/tmp/brdetect/demo.dmg</string>
			<key>image-type</key>
			<string>UDIF read-only compressed (zlib)</string>
			<key>owner-uid</key>
			<integer>501</integer>
			<key>removable</key>
			<true/>
			<key>system-entities</key>
			<array>
				<dict>
					<key>content-hint</key>
					<string>GUID_partition_scheme</string>
					<key>dev-entry</key>
					<string>/dev/disk6</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>7C3457EF-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk6s1</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>EF57347C-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk9</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>41504653-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk9s1</string>
					<key>mount-point</key>
					<string>/Volumes/bladerunner-demo</string>
				</dict>
			</array>
			<key>writeable</key>
			<false/>
		</dict>
		<dict>
			<key>autodiskmount</key>
			<false/>
			<key>blockcount</key>
			<integer>25165824</integer>
			<key>blocksize</key>
			<integer>512</integer>
			<key>diskimages2</key>
			<false/>
			<key>hdid-pid</key>
			<integer>28139</integer>
			<key>icon-path</key>
			<string>/System/Library/PrivateFrameworks/DiskImages.framework/Resources/CDiskImage.icns</string>
			<key>image-encrypted</key>
			<false/>
			<key>image-path</key>
			<string>/private/tmp/brdetect/work.sparseimage</string>
			<key>image-type</key>
			<string>sparse disk image</string>
			<key>owner-uid</key>
			<integer>501</integer>
			<key>removable</key>
			<true/>
			<key>system-entities</key>
			<array>
				<dict>
					<key>content-hint</key>
					<string>GUID_partition_scheme</string>
					<key>dev-entry</key>
					<string>/dev/disk10</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>C12A7328-F81F-11D2-BA4B-00A0C93EC93B</string>
					<key>dev-entry</key>
					<string>/dev/disk10s1</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>7C3457EF-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk10s2</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>EF57347C-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk11</string>
				</dict>
				<dict>
					<key>content-hint</key>
					<string>41504653-0000-11AA-AA11-00306543ECAC</string>
					<key>dev-entry</key>
					<string>/dev/disk11s1</string>
				</dict>
			</array>
			<key>writeable</key>
			<true/>
		</dict>
	</array>
	<key>revision</key>
	<string>683.100.3</string>
	<key>vendor</key>
	<string>Apple</string>
</dict>
</plist>
`

// emptyInfoPlist is `hdiutil info -plist` with nothing attached: a well-formed
// plist that simply omits the images key.
const emptyInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>framework</key>
	<string>683.100.3</string>
	<key>revision</key>
	<string>683.100.3</string>
	<key>vendor</key>
	<string>Apple</string>
</dict>
</plist>
`

// Paths and device nodes from sampleInfoPlist, named so the expectations read
// as intent rather than as strings.
const (
	sampleCartridgeDMG    = "/private/tmp/brdetect/demo.dmg"
	sampleCartridgeMount  = "/Volumes/bladerunner-demo"
	sampleCartridgeDev    = "/dev/disk9s1"
	sampleCartridgeWhole  = "/dev/disk6"
	sampleSparseImage     = "/private/tmp/brdetect/work.sparseimage"
	sampleSparseDev       = "/dev/disk11s1"
	sampleUnrelatedDMG    = "/Users/someone/build/Other_0.0.0_aarch64.dmg"
	sampleUnrelatedMount  = "/Volumes/dmg.mrluVn"
	sampleReadOnlyDMGType = "UDIF read-only compressed (zlib)"
)

// infoRunner returns a fake hdiutil whose info output is the given plist.
func infoRunner(plist string) *fakeRunner {
	return &fakeRunner{results: []fakeResult{{stdout: plist}}}
}

func TestParseInfoImagesFromRealCapture(t *testing.T) {
	images, err := parseInfoImages(sampleInfoPlist)
	if err != nil {
		t.Fatalf("parseInfoImages: %v", err)
	}
	if len(images) != 3 {
		t.Fatalf("images = %d, want 3: %+v", len(images), images)
	}

	wantPaths := []string{sampleUnrelatedDMG, sampleCartridgeDMG, sampleSparseImage}
	wantEntities := []int{2, 4, 5}
	wantWritable := []bool{true, false, true}
	for i, img := range images {
		if img.path != wantPaths[i] {
			t.Errorf("images[%d].path = %q, want %q", i, img.path, wantPaths[i])
		}
		if len(img.entities) != wantEntities[i] {
			t.Errorf("images[%d] entities = %d, want %d", i, len(img.entities), wantEntities[i])
		}
		if img.writable != wantWritable[i] {
			t.Errorf("images[%d].writable = %v, want %v", i, img.writable, wantWritable[i])
		}
		if !img.removable {
			t.Errorf("images[%d].removable = false, want true", i)
		}
	}
	if got := images[1].imageType; got != sampleReadOnlyDMGType {
		t.Errorf("cartridge imageType = %q, want %q", got, sampleReadOnlyDMGType)
	}
	if got := images[2].firstMountpoint(); got != "" {
		t.Errorf("a -nomount image reports mountpoint %q, want none", got)
	}
}

func TestParseInfoImagesWithNothingAttached(t *testing.T) {
	images, err := parseInfoImages(emptyInfoPlist)
	if err != nil {
		t.Fatalf("an empty attach list is not an error: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %+v, want none", images)
	}
}

func TestParseInfoImagesRejectsNonPlist(t *testing.T) {
	if _, err := parseInfoImages("hdiutil: info: unrecognized option\n"); err == nil {
		t.Fatal("parseInfoImages accepted non-plist output")
	}
}

func TestBackingImageForResolvesEveryAddressingForm(t *testing.T) {
	tests := []struct {
		name           string
		ref            string
		wantImage      string
		wantDev        string
		wantMountpoint string
		wantWritable   bool
	}{
		{
			name:           "mounted volume device node",
			ref:            sampleCartridgeDev,
			wantImage:      sampleCartridgeDMG,
			wantDev:        sampleCartridgeDev,
			wantMountpoint: sampleCartridgeMount,
		},
		{
			name:           "bare BSD name as DiskArbitration reports it",
			ref:            "disk9s1",
			wantImage:      sampleCartridgeDMG,
			wantDev:        sampleCartridgeDev,
			wantMountpoint: sampleCartridgeMount,
		},
		{
			name:           "whole disk device node falls back to the mounted entity",
			ref:            sampleCartridgeWhole,
			wantImage:      sampleCartridgeDMG,
			wantDev:        sampleCartridgeWhole,
			wantMountpoint: sampleCartridgeMount,
		},
		{
			name:           "mountpoint",
			ref:            sampleCartridgeMount,
			wantImage:      sampleCartridgeDMG,
			wantDev:        sampleCartridgeDev,
			wantMountpoint: sampleCartridgeMount,
		},
		{
			name:           "an unrelated image is not confused with the cartridge",
			ref:            sampleUnrelatedMount,
			wantImage:      sampleUnrelatedDMG,
			wantDev:        "/dev/disk4s1",
			wantMountpoint: sampleUnrelatedMount,
			wantWritable:   true,
		},
		{
			name:         "image attached with -nomount is found by device node",
			ref:          sampleSparseDev,
			wantImage:    sampleSparseImage,
			wantDev:      sampleSparseDev,
			wantWritable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := backingImageFor(context.Background(), infoRunner(sampleInfoPlist), tt.ref)
			if err != nil {
				t.Fatalf("backingImageFor(%q): %v", tt.ref, err)
			}
			if got.ImagePath != tt.wantImage {
				t.Errorf("ImagePath = %q, want %q", got.ImagePath, tt.wantImage)
			}
			if got.DevNode != tt.wantDev {
				t.Errorf("DevNode = %q, want %q", got.DevNode, tt.wantDev)
			}
			if got.Mountpoint != tt.wantMountpoint {
				t.Errorf("Mountpoint = %q, want %q", got.Mountpoint, tt.wantMountpoint)
			}
			if got.Writable != tt.wantWritable {
				t.Errorf("Writable = %v, want %v", got.Writable, tt.wantWritable)
			}
		})
	}
}

func TestBackingImageForUsesInfoPlist(t *testing.T) {
	f := infoRunner(sampleInfoPlist)
	if _, err := backingImageFor(context.Background(), f, sampleCartridgeDev); err != nil {
		t.Fatalf("backingImageFor: %v", err)
	}
	want := append([]string{hdiutil}, infoArgs()...)
	if !argsEqual(f.lastCall(), want) {
		t.Fatalf("hdiutil call = %v, want %v", f.lastCall(), want)
	}
}

func TestBackingImageForUnknownReference(t *testing.T) {
	tests := []struct{ name, ref string }{
		{"unrelated mountpoint", "/Volumes/Time Machine"},
		{"unrelated device", "/dev/disk0s2"},
		{"an unmounted image is never matched by mountpoint", "/private/tmp/brdetect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := backingImageFor(context.Background(), infoRunner(sampleInfoPlist), tt.ref)
			if !errors.Is(err, ErrNoBackingImage) {
				t.Fatalf("err = %v, want ErrNoBackingImage", err)
			}
		})
	}
}

func TestBackingImageForEmptyReference(t *testing.T) {
	_, err := backingImageFor(context.Background(), infoRunner(sampleInfoPlist), "")
	if !errors.Is(err, ErrNoImageRef) {
		t.Fatalf("err = %v, want ErrNoImageRef", err)
	}
}

func TestBackingImageForPropagatesHdiutilFailure(t *testing.T) {
	wantErr := errors.New("exit status 1")
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: info failed", err: wantErr}}}
	_, err := backingImageFor(context.Background(), f, sampleCartridgeDev)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
}

func TestListAttachedImagesReportsEveryImagePath(t *testing.T) {
	images, err := listAttachedImages(context.Background(), infoRunner(sampleInfoPlist))
	if err != nil {
		t.Fatalf("listAttachedImages: %v", err)
	}
	want := []string{sampleUnrelatedDMG, sampleCartridgeDMG, sampleSparseImage}
	got := make([]string, 0, len(images))
	for _, img := range images {
		got = append(got, img.path)
	}
	if !argsEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

// The BSD device-name rule this package resolves a lookup with is
// internal/diskarb's, not a local copy. The table therefore covers every
// spelling macOS uses for a device — including the raw "/dev/rdiskN" form the
// deleted local copy did NOT recognize — and every spelling that is not a
// device, including a MOUNTPOINT whose last element reads like a device name.
//
// wantDev is the canonical node the rule reduces ref to ("" when ref names no
// device), and wantImage is the attached image matchImage then finds ("" when
// nothing attached sits at that node).
func TestMatchImageResolvesEverySpellingThroughDiskarb(t *testing.T) {
	images, err := parseInfoImages(sampleInfoPlist)
	if err != nil {
		t.Fatalf("parseInfoImages: %v", err)
	}

	tests := []struct {
		name      string
		ref       string
		wantDev   string
		wantImage string
	}{
		{name: "bare whole disk", ref: "disk4", wantDev: "/dev/disk4", wantImage: sampleUnrelatedDMG},
		{name: "bare slice", ref: "disk4s1", wantDev: "/dev/disk4s1", wantImage: sampleUnrelatedDMG},
		{name: "block device path", ref: "/dev/disk4s1", wantDev: "/dev/disk4s1", wantImage: sampleUnrelatedDMG},
		// The local copy read this as a mountpoint and matched nothing.
		{name: "raw device path", ref: "/dev/rdisk4s1", wantDev: "/dev/disk4s1", wantImage: sampleUnrelatedDMG},
		{name: "apfs sub slice", ref: "disk4s1s2", wantDev: "/dev/disk4s1s2", wantImage: ""},
		{name: "empty", ref: "", wantDev: "", wantImage: ""},
		{name: "not a device at all", ref: "not-a-device", wantDev: "", wantImage: ""},
		// A volume may be named after a device. The directory decides.
		{name: "mountpoint that reads like a device", ref: "/Volumes/disk4", wantDev: "", wantImage: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diskarb.DevPath(tt.ref); got != tt.wantDev {
				t.Errorf("diskarb.DevPath(%q) = %q, want %q", tt.ref, got, tt.wantDev)
			}
			got, ok := matchImage(images, tt.ref)
			if tt.wantImage == "" {
				if ok {
					t.Fatalf("matchImage(%q) = %+v, want no match", tt.ref, got)
				}
				return
			}
			if !ok {
				t.Fatalf("matchImage(%q) found nothing, want %s", tt.ref, tt.wantImage)
			}
			if got.ImagePath != tt.wantImage {
				t.Errorf("matchImage(%q).ImagePath = %q, want %q", tt.ref, got.ImagePath, tt.wantImage)
			}
			if got.DevNode != tt.wantDev {
				t.Errorf("matchImage(%q).DevNode = %q, want %q", tt.ref, got.DevNode, tt.wantDev)
			}
		})
	}
}

// devLiteralAllowList holds the string literals in this package that may
// mention a device without being a second copy of the rule. It is empty: there
// is no such literal, and a new one must be justified by editing this list.
var devLiteralAllowList = map[string]bool{}

// This package must never grow its own BSD device-name parsing again. It did
// once, the copy disagreed with internal/diskarb on the raw-device spelling,
// and a divergent copy of this rule is what shipped an unmount veto registered
// under a name DiskArbitration could never match.
//
// A copy needs the vocabulary: the "/dev" directory, or the bare "disk" /
// "rdisk" prefix. This test reads the package's own source and fails on any of
// them, so the reintroduction is caught at `make test` rather than at review.
func TestPackageDeclaresNoDeviceNameVocabulary(t *testing.T) {
	sources, err := packageSourceFiles(".")
	if err != nil {
		t.Fatalf("list package source: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("found no package source; the guard would pass vacuously")
	}

	fset := token.NewFileSet()
	for _, name := range sources {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, lit := range deviceVocabularyLiterals(file) {
			t.Errorf("%s declares the device-name literal %s; call internal/diskarb instead "+
				"(BSDName, WholeDiskName, DevPath)", name, lit)
		}
	}
}

// packageSourceFiles lists the package's own .go files in dir, excluding the
// test files (this one holds device literals on purpose, as fixtures). Build
// tags are deliberately ignored: a copy of the rule in a _darwin.go file is
// still a copy, and the guard must see it from the Linux test run too.
func packageSourceFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sources = append(sources, filepath.Join(dir, name))
	}
	return sources, nil
}

// deviceVocabularyLiterals returns every string literal in file that names the
// device directory or a BSD name prefix. Import paths are skipped — the point
// is to REQUIRE importing internal/diskarb, not to forbid naming it.
func deviceVocabularyLiterals(file *ast.File) []string {
	imports := make(map[*ast.BasicLit]bool, len(file.Imports))
	for _, spec := range file.Imports {
		imports[spec.Path] = true
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || imports[lit] {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil || devLiteralAllowList[value] {
			return true
		}
		if strings.Contains(value, devDirFragment) || value == bsdPrefix || value == rawBSDPrefix {
			found = append(found, lit.Value)
		}
		return true
	})
	return found
}

// The vocabulary a reintroduced copy of the rule would have to spell out.
const (
	devDirFragment = "/dev"
	bsdPrefix      = "disk"
	rawBSDPrefix   = "rdisk"
)
