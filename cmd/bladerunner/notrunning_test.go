package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// The most-hit error in the CLI told every caller to run 'br start'. For a disk
// slot or a cartridge that is wrong and makes things worse: 'br start' creates
// an ADDITIONAL flat VM instead of bringing back the instance the user meant.
// The message must name the verb that restores the actual target, and it must
// point at 'br instances'.
func TestNotRunningErrorNamesTheVerbThatBringsTheInstanceBack(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	cases := []struct {
		name    string
		target  resolvedInstance
		want    []string
		notWant []string
	}{
		{
			name: "nothing is running at all",
			target: resolvedInstance{
				Name: config.DefaultInstanceName, Kind: instance.KindFlat,
				StateDir: root, Fallback: true,
			},
			want: []string{"no VM is running", "br up", "br boot", "br instances"},
		},
		{
			name: "the default instance is not running",
			target: resolvedInstance{
				Name: config.DefaultInstanceName, Kind: instance.KindFlat,
				StateDir: root, Explicit: true,
			},
			want: []string{"default", "not running", "br up", "br instances"},
		},
		{
			name: "a disk slot is booted by name",
			target: resolvedInstance{
				Name: "demo", Kind: instance.KindDisk,
				StateDir: filepath.Join(root, disksDirName, "demo"), Explicit: true,
			},
			want:    []string{`"demo"`, "disk", "not running", "br boot demo", "br instances"},
			notWant: []string{"br start"},
		},
		{
			name: "a cartridge is booted from its image",
			target: resolvedInstance{
				Name: "demo", Kind: instance.KindCartridge,
				StateDir: "/Volumes/bladerunner-demo", SourcePath: "/Volumes/ship/demo.dmg",
				Explicit: true,
			},
			want:    []string{`"demo"`, "cartridge", "br boot /Volumes/ship/demo.dmg", "br instances"},
			notWant: []string{"br start"},
		},
		{
			name: "a cartridge with no recorded image falls back to its name",
			target: resolvedInstance{
				Name: "demo", Kind: instance.KindCartridge,
				StateDir: "/Volumes/bladerunner-demo", Explicit: true,
			},
			want:    []string{"br boot demo"},
			notWant: []string{"br start"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := notRunningError(tc.target)
			if !errors.Is(err, errVMNotRunning) {
				t.Errorf("notRunningError does not wrap errVMNotRunning: %v", err)
			}
			got := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("error = %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("error = %q, want it NOT to contain %q", got, notWant)
				}
			}
		})
	}
}
