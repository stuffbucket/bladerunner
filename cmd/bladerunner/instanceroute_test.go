package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// selectInstance points every verb at name for the duration of the test, the
// way `--instance name` does on the command line.
func selectInstance(t *testing.T, name string) {
	t.Helper()
	saved := instanceFlag
	instanceFlag = name
	t.Cleanup(func() { instanceFlag = saved })
}

// scratchInstance gives the test its own state dir holding one registered but
// NOT running instance, so every verb resolves it and then fails to reach it.
// The failure is the observation: a verb that ignores --instance reports the
// flat default instead of the instance the user named.
func scratchInstance(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	t.Setenv(instanceEnvVar, "")
	t.Setenv(instanceEnvVarAlias, "")

	slot := filepath.Join(root, disksDirName, name)
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatalf("create disk slot %q: %v", name, err)
	}
	register(t, root, instance.Entry{Name: name, Kind: instance.KindDisk, StateDir: slot})
	return root
}

// Every verb that acts on a running VM must act on the instance the user
// selected. Nothing is running in this state dir, so the proof is in the
// message: it has to name the selected instance rather than fall back to the
// flat default (issue #9).
func TestVerbsActOnTheSelectedInstance(t *testing.T) {
	const selected = "demo"

	cases := []struct {
		verb string
		run  func() error
	}{
		{verb: "shell", run: func() error { return runShell(shellCmd, nil) }},
		{verb: "incus", run: func() error { return runIncus(incusCmd, []string{"list"}) }},
		{verb: "ssh", run: func() error { return runSSH(sshCmd, nil) }},
		{verb: "ls", run: func() error { return runLs(lsCmd, nil) }},
		{verb: "logs", run: func() error { return runLogs(logsCmd, []string{"box"}) }},
		{verb: "events", run: func() error { return runEvents(eventsCmd, nil) }},
		{verb: "reconnect", run: func() error { return runReconnect(reconnectCmd, nil) }},
		{verb: "save", run: func() error { return runSave(saveCmd, nil) }},
		{verb: "web", run: func() error { return runWeb(webCmd, nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			scratchInstance(t, selected)
			selectInstance(t, selected)

			err := tc.run()
			if err == nil {
				t.Fatalf("br %s: no error, want one naming instance %q", tc.verb, selected)
			}
			if !strings.Contains(err.Error(), selected) {
				t.Errorf("br %s: error = %q, want it to name the selected instance %q", tc.verb, err, selected)
			}
		})
	}
}

// `br restore` and `br upgrade` hand the instance to runStart, which can only
// rebuild the flat default's specification. They must say so rather than act on
// the default while the user named something else.
func TestRestoreAndUpgradeRefuseANamedInstance(t *testing.T) {
	cases := []struct {
		verb string
		run  func() error
	}{
		{verb: "restore", run: func() error { return runRestore(restoreCmd, nil) }},
		{verb: "upgrade", run: func() error { return runUpgrade(upgradeCmd, nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			scratchInstance(t, "demo")
			selectInstance(t, "demo")

			err := tc.run()
			if err == nil {
				t.Fatalf("br %s --instance demo: no error, want a refusal", tc.verb)
			}
			if !strings.Contains(err.Error(), "demo") || !strings.Contains(err.Error(), "default") {
				t.Errorf("br %s --instance demo: error = %q, want it to name both the instance and the default", tc.verb, err)
			}
		})
	}
}

// With no instance selected, restore and upgrade keep acting on the flat
// default exactly as they always have.
func TestRestoreAcceptsTheDefaultInstance(t *testing.T) {
	root := scratchInstance(t, "demo")
	selectInstance(t, "")

	stateDir, err := requireDefaultInstance("restore")
	if err != nil {
		t.Fatalf("requireDefaultInstance with no selection returned error: %v", err)
	}
	if filepath.Clean(stateDir) != filepath.Clean(root) {
		t.Errorf("requireDefaultInstance = %q, want the default state dir %q", stateDir, root)
	}

	selectInstance(t, defaultSlotAlias)
	if _, err := requireDefaultInstance("restore"); err != nil {
		t.Errorf("requireDefaultInstance with --instance %s returned error: %v", defaultSlotAlias, err)
	}
}
