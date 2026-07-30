package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The build script accepts WORK_DIR from the environment and registers
// `trap 'rm -rf "${WORK_DIR}"' EXIT`. It never establishes that an overridden
// directory was created by this invocation, so pointing WORK_DIR at a CI
// workspace, a home directory, or a mistyped path recursively deletes it.
//
// This exercises the cheapest path that reaches the trap: invoking the script
// with no arguments prints usage and exits non-zero, and the EXIT trap still
// runs. A caller-supplied directory and its contents must survive. Reported as
// #242.
func TestBuildScriptDoesNotDeleteACallerOwnedWorkDir(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := repoBuildScript(t)

	work := t.TempDir()
	sentinel := filepath.Join(work, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("caller data"), 0o600); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	// No arguments: the script prints usage, exits non-zero, and the EXIT trap
	// fires. Failure here is the point of the test.
	cmd := exec.Command(bash, script)
	cmd.Env = append(os.Environ(), "WORK_DIR="+work)
	_ = cmd.Run() // non-zero exit is expected

	if _, err := os.Stat(work); os.IsNotExist(err) {
		t.Fatalf("the script deleted the caller-supplied WORK_DIR %s", work)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("the script destroyed caller data in WORK_DIR: %v", err)
	}
}

// A work directory the script created itself must still be removed, or every
// build leaks a multi-gigabyte temporary tree. Pointing TMPDIR at a directory
// this test owns makes the script's own mktemp -d land somewhere observable.
func TestBuildScriptStillCleansItsOwnWorkDir(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := repoBuildScript(t)

	tmp := t.TempDir()
	cmd := exec.Command(bash, script)
	// WORK_DIR unset, so the script creates its own under TMPDIR.
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "WORK_DIR=") && !strings.HasPrefix(e, "TMPDIR=") {
			env = append(env, e)
		}
	}
	env = append(env, "TMPDIR="+tmp)
	cmd.Env = env
	_ = cmd.Run() // usage error is expected

	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read TMPDIR: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("script left %d entries in TMPDIR (%v); its own work dir must be cleaned up", len(left), names)
	}
}

// repoBuildScript locates the build script relative to this package, rather
// than through resolveBuildScript, whose cwd search does not find it when the
// test binary runs from the package directory.
func repoBuildScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "build-guest-image.sh")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("build script not present at %s: %v", path, err)
	}
	return path
}
