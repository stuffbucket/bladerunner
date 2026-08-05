package main

import (
	"os"
	"strings"
	"testing"
)

// `br ssh` must CONNECT, not print.
//
// Every comparable tool — colima, multipass, vagrant — spells the connecting
// verb `ssh`. This one printed connection details and left you where you
// started, which is a surprise each new user pays for once. It is an alias of
// shell rather than a second command, so the two cannot drift.
func TestSSHResolvesToTheConnectingVerb(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"ssh"})
	if err != nil {
		t.Fatalf("br ssh does not resolve: %v", err)
	}
	if cmd.Name() != "shell" {
		t.Errorf("br ssh resolves to %q, want shell — ssh must connect, not print", cmd.Name())
	}
}

// The printing verb must still exist, under colima's name for it.
func TestSSHConfigStillPrintsTheDetails(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"ssh-config"})
	if err != nil {
		t.Fatalf("br ssh-config does not resolve: %v", err)
	}
	if cmd.Name() != "ssh-config" {
		t.Fatalf("resolved %q, want ssh-config", cmd.Name())
	}
}

// The two must not be presented as if they were different ways to connect.
//
// The start banner and status footer print a Shell: line and an SSH: line side
// by side. Once ssh became an alias of shell, pointing both at connecting verbs
// printed the same command twice under two labels.
func TestStatusAndStartDoNotOfferTheSameCommandTwice(t *testing.T) {
	for _, f := range []string{"status.go", "start.go"} {
		body := readSource(t, f)
		if strings.Contains(body, `command("br ssh")`) {
			t.Errorf(`%s still offers command("br ssh") beside br shell; they are now the same verb`, f)
		}
	}
}

// readSource reads a file from this package's own directory.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
