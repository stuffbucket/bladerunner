package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getStartedVerb is what a user with no VM should be told to run.
//
// `br up` starts one with defaults and then shows the next steps; `br start` is
// the flag-taking form for when the shape matters. The root help names `br up`
// under "Getting started", so every other place that answers "how do I get a
// VM" has to agree with it.
const getStartedVerb = "br up"

// No user-facing hint may tell someone to run `br start` to get a VM.
//
// Five places did, each with its own wording, while notRunningError had already
// moved to `br up` — and its comment explains why the old advice is not merely
// inconsistent but harmful: for a disk slot or a cartridge, `br start` creates
// an ADDITIONAL flat VM rather than bringing back the instance the user meant,
// so following it leaves two VMs where one was wanted and the original down.
//
// Derived from the sources rather than listing the five, so a sixth is caught
// the day it is written.
func TestNoHintSendsUsersToBrStartForAVM(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++

		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, `command("br start")`) {
				continue
			}
			t.Errorf("%s:%d offers `br start` as the way to get a VM; use %q.\n"+
				"  For a disk slot or cartridge that advice creates a SECOND VM "+
				"and leaves the original down (see notRunningError).\n  %s",
				name, i+1, getStartedVerb, strings.TrimSpace(line))
		}
	}

	if checked == 0 {
		t.Fatal("scanned no source files; this check has stopped checking anything")
	}
}

// The root help and the idle status must name the same verb.
//
// A new user meets both, and being told `br up` in one and `br start` in the
// other is the moment they stop trusting either.
func TestGettingStartedVerbIsConsistent(t *testing.T) {
	if !strings.Contains(rootCmd.Long, getStartedVerb) {
		t.Errorf("root help does not name %q under Getting started:\n%s", getStartedVerb, rootCmd.Long)
	}

	status := readSource(t, "status.go")
	if !strings.Contains(status, `command("br up")`) {
		t.Errorf("the idle status does not point at %q, so it disagrees with the root help", getStartedVerb)
	}
}
