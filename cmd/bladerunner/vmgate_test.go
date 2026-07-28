package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

func TestConfirmStartVMFrom(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty default yes", "\n", true},
		{"eof default yes", "", true},
		{"y", "y\n", true},
		{"yes", "yes\n", true},
		{"uppercase Y", "Y\n", true},
		{"yes with spaces", "  yes  \n", true},
		{"n", "n\n", false},
		{"no", "no\n", false},
		{"garbage", "maybe\n", false},
		{"explicit n no newline", "n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := confirmStartVMFrom(strings.NewReader(tc.input))
			if got != tc.want {
				t.Fatalf("confirmStartVMFrom(%q)=%v want %v", tc.input, got, tc.want)
			}
		})
	}
}

// The holder's command line is what actually configures a detached process, so
// pin it exactly. It is now a single hand-off file: everything else about the
// instance travels inside it, because the ordinary boot paths configure far
// more than a state directory and mirroring each knob as a `br vmd` flag would
// have grown the holder into a second copy of the `br start` flag set.
func TestHolderSpawnArgs(t *testing.T) {
	spawn := holderSpawn{Spec: vmhost.Spec{Kind: instance.KindFlat, StateDir: "/s"}}
	if got, want := strings.Join(spawn.args("/s/vmd.json"), " "), "vmd --spec /s/vmd.json"; got != want {
		t.Fatalf("args() = %q, want %q", got, want)
	}
}

// The hand-off file sits beside the holder log and is keyed the same way, so
// the several cartridges that share the registry root as their state directory
// cannot overwrite one another's.
func TestHolderSpawnSpecPathIsPerInstance(t *testing.T) {
	root := t.TempDir()
	spawns := []holderSpawn{
		{Spec: vmhost.Spec{StateDir: root}},
		{Spec: vmhost.Spec{StateDir: root, CartridgePath: "/img/demo.dmg"}},
		{Spec: vmhost.Spec{StateDir: root, CartridgePath: "/img/other.dmg"}},
		{Spec: vmhost.Spec{StateDir: root, Name: "named", CartridgePath: "/img/demo.dmg"}},
	}
	seen := map[string]bool{}
	for _, s := range spawns {
		path := s.specPath()
		if seen[path] {
			t.Errorf("spawn %+v reuses the hand-off file %q", s.Spec, path)
		}
		if filepath.Dir(path) != root {
			t.Errorf("hand-off file %q escaped %q", path, root)
		}
		seen[path] = true
	}
	// A name that is not a usable path element must not be pasted into a path.
	escape := holderSpawn{Spec: vmhost.Spec{StateDir: root, Name: "../../etc/passwd"}}
	if got := escape.specPath(); filepath.Dir(got) != root {
		t.Errorf("hand-off file %q escaped %q", got, root)
	}
}

// The Spec is the whole contract between the spawner and the holder, so the
// write/read pair has to be lossless — and the read has to CONSUME the file, so
// a state directory does not accumulate one per boot.
func TestHolderSpecHandoffRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := vmhost.Spec{
		Kind:          instance.KindCartridge,
		Name:          "demo",
		StateDir:      root,
		CartridgePath: "/img/demo.dmg",
		Persist:       true,
		MountPolicy:   cartridge.MountPrivate,
		Mountpoint:    filepath.Join(root, "mnt", "demo"),
		Overrides:     vmhost.Overrides{CPUs: 6, MemoryGiB: 12, Timeout: 4 * time.Minute},
		ChangedFlags:  []string{"cpus", "memory", "timeout"},
		BinaryVersion: "test",
	}
	path, err := writeHolderSpec(holderSpawn{Spec: want})
	if err != nil {
		t.Fatalf("writeHolderSpec: %v", err)
	}
	got, err := readHolderSpec(path)
	if err != nil {
		t.Fatalf("readHolderSpec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hand-off changed the spec:\n got %+v\nwant %+v", got, want)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hand-off file survived the read: %v", err)
	}
}

// A hand-off file that cannot be parsed is KEPT, because it is the one thing a
// human debugging a holder that never started would want to look at.
func TestReadHolderSpecKeepsAnUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmd.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readHolderSpec(path); err == nil {
		t.Fatal("readHolderSpec accepted a corrupt file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("corrupt hand-off file was removed: %v", err)
	}
}

// A holder without a state directory is a bug in the spawner, and it is
// rejected before anything is executed or any file is created.
func TestSpawnHolderRequiresAStateDir(t *testing.T) {
	if _, err := spawnHolder(holderSpawn{}); !errors.Is(err, errHolderStateDir) {
		t.Fatalf("spawnHolder() error = %v, want errHolderStateDir", err)
	}
}

// spawnDetached refuses to run without somewhere to send the child's output:
// inheriting the parent's stdio is exactly what stops a holder from surviving
// the parent.
func TestSpawnDetachedRequiresStdio(t *testing.T) {
	if _, err := spawnDetached(detachedSpawn{Args: []string{"vmd"}}); err == nil {
		t.Fatal("spawnDetached with no stdio must fail")
	}
}
