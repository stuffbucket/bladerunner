package main

import (
	"errors"
	"strings"
	"testing"
	"time"
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
// pin it exactly: --state-dir is always present, and every optional flag is
// emitted only when it carries a value.
func TestHolderSpawnArgs(t *testing.T) {
	cases := []struct {
		name  string
		spawn holderSpawn
		want  string
	}{
		{
			name:  "state dir only",
			spawn: holderSpawn{StateDir: "/s"},
			want:  "vmd --state-dir /s",
		},
		{
			name:  "cartridge",
			spawn: holderSpawn{StateDir: "/s", CartridgePath: "/c/demo.dmg"},
			want:  "vmd --state-dir /s --cartridge /c/demo.dmg",
		},
		{
			name:  "named gui holder",
			spawn: holderSpawn{StateDir: "/s", Name: "demo", GUI: true},
			want:  "vmd --state-dir /s --name demo --gui",
		},
		{
			name:  "drain timeout",
			spawn: holderSpawn{StateDir: "/s", DrainTimeout: 90 * time.Second},
			want:  "vmd --state-dir /s --drain-timeout 1m30s",
		},
		{
			name: "everything",
			spawn: holderSpawn{
				StateDir:      "/s",
				CartridgePath: "/c/demo.dmg",
				Name:          "demo",
				GUI:           true,
				DrainTimeout:  30 * time.Second,
			},
			want: "vmd --state-dir /s --cartridge /c/demo.dmg --name demo --gui --drain-timeout 30s",
		},
		{
			name:  "zero drain timeout is left to the holder default",
			spawn: holderSpawn{StateDir: "/s", DrainTimeout: 0},
			want:  "vmd --state-dir /s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.spawn.args(), " "); got != tc.want {
				t.Fatalf("args() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A holder without a state directory is a bug in the spawner, and it is
// rejected before anything is executed or any log file is created.
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
