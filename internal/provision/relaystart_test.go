package provision

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// Starting the relays must not BLOCK on their backends.
//
// The units spin-wait in ExecStartPre for a backend port — ssh on 22, incus on
// 8443 — and incus is installed by the NEXT stage of the same bootstrap script.
// A blocking `systemctl enable --now` therefore waits for a port that cannot
// appear until after the command returns, and only systemd's default 90s
// TimeoutStartSec breaks it. `|| true` then swallows the failure, so it looked
// like it worked while costing 92 measured seconds of every first boot.
//
// Measured: a 194s cold boot with 92s between the ssh-up and vsock-services-up
// stages, against ~14s for everything Incus actually does.
func TestRelayStartDoesNotBlockOnBackends(t *testing.T) {
	script := renderVsockRelays(&config.Config{})

	line := enableLine(t, script)
	if !strings.Contains(line, "--no-block") {
		t.Errorf("the relays are started with a blocking systemctl:\n  %s\n"+
			"A channel whose backend comes up later in this same script will "+
			"stall the boot for systemd's 90s start timeout.", line)
	}
}

// The guard above only means something while a channel actually spin-waits. If
// the waits were ever removed, --no-block would stop protecting anything and
// this test would keep passing for the wrong reason.
func TestSomeRelayChannelStillWaitsForItsBackend(t *testing.T) {
	var waiting []string
	for _, ch := range relayChannels(&config.Config{}) {
		if ch.wait != "" {
			waiting = append(waiting, ch.name)
		}
	}
	if len(waiting) == 0 {
		t.Fatal("no relay channel spin-waits any more, so the --no-block assertion " +
			"above no longer protects against anything; re-derive it or delete it")
	}
	t.Logf("channels that spin-wait for a backend: %v", waiting)
}

// The incus relay is the one that cannot possibly be satisfied at start time.
//
// Pinned by name because it is the specific case that cost 92s: its backend is
// installed by a later stage of the same script.
func TestIncusRelayWaitsForAPortInstalledLater(t *testing.T) {
	for _, ch := range relayChannels(&config.Config{}) {
		if ch.name != "incus" {
			continue
		}
		if ch.wait == "" {
			t.Skip("the incus relay no longer waits for a backend")
		}
		return
	}
	t.Error("no incus relay channel found; the 92s first-boot stall was this channel")
}

// enableLine returns the systemctl line that starts the relay instances.
func enableLine(t *testing.T, script string) string {
	t.Helper()
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(l, "systemctl enable") {
			return l
		}
	}
	t.Fatal("the relay setup never enables the units")
	return ""
}
