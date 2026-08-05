package main

import (
	"strings"
	"testing"
)

// Restart must be registered on the root command, in the lifecycle group.
//
// A verb that exists but is not wired reads as missing, which is the state this
// command was added to fix.
func TestRestartIsRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"restart"})
	if err != nil {
		t.Fatalf("br restart is not registered: %v", err)
	}
	if cmd.Name() != "restart" {
		t.Fatalf("resolved %q, want restart", cmd.Name())
	}
	if cmd.GroupID != groupLifecycle {
		t.Errorf("GroupID = %q, want %q so it renders beside start and stop", cmd.GroupID, groupLifecycle)
	}
}

// Restart must carry the stop half's escape hatches.
//
// The stop is the half that can hang on a wedged guest, and someone reaching
// for restart to recover from exactly that needs the same way out `br stop`
// gives them.
func TestRestartCarriesTheStopEscapeHatches(t *testing.T) {
	for _, name := range []string{"timeout", "force"} {
		if restartCmd.Flags().Lookup(name) == nil {
			t.Errorf("br restart has no --%s, so a wedged guest cannot be recovered with it", name)
		}
	}
}

// Restart must bring the VM back on SETTINGS, not on start's flag defaults.
//
// runStart reads startFlags, which are bound to startCmd's flag set. Those
// variables still hold the defaults pflag wrote at registration, so a restart
// that reported them as "changed" would silently reshape the VM — a restart
// that quietly moves a guest from 8 CPUs to 4 is worse than no restart at all.
//
// What prevents it is that changedStartFlags asks the COMMAND being run, and
// restart's flag set has none of those flags. This pins that.
func TestRestartChangesNoStartFlags(t *testing.T) {
	if got := changedStartFlags(restartCmd); len(got) != 0 {
		t.Errorf("changedStartFlags(restartCmd) = %v, want none: a restart must not "+
			"apply start's flag defaults as if the user had asked for them", got)
	}

	// Guard the guard: if startFlagNames were ever emptied, the assertion above
	// would hold for the wrong reason and stop protecting anything.
	if len(startFlagNames) == 0 {
		t.Fatal("startFlagNames is empty, so the assertion above proves nothing")
	}
	if got := changedStartFlags(startCmd); got == nil && len(startFlagNames) > 0 {
		t.Log("startCmd reports no changed flags, which is expected when nothing was parsed")
	}
}

// The help must say that one-off start flags do not survive a restart.
//
// They genuinely do not — start does not persist them — and a user who set
// --cpus 8 once will otherwise read "restart" as "put it back how it was".
func TestRestartHelpWarnsThatStartFlagsDoNotSurvive(t *testing.T) {
	long := restartCmd.Long
	for _, want := range []string{"not persisted", "br config"} {
		if !strings.Contains(long, want) {
			t.Errorf("restart help does not mention %q:\n%s", want, long)
		}
	}
}
