package provision

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// Provisioning assets embedded from the checked-in files under
// internal/provision/scripts/. Those files are the SINGLE SOURCE OF TRUTH: the
// cloud-init path emits these bytes (verbatim for the watchdog/chrony assets;
// the relay template is instantiated per channel). After #160 every boot
// provisions via full cloud-init, so cloud-init is the sole runtime source for
// the vsock relays; the image build (scripts/build-guest-image.sh) still
// --copy-ins the watchdog + chrony.conf so the time stack is present pre-boot.
//
// The embed paths are relative to this package directory and cannot traverse
// upward, which is why the assets live in internal/provision/scripts/ rather
// than the repo-root scripts/.

// watchdogScript is the guest-local wake-heal watchdog body. Emitted verbatim by
// the cloud-init path and --copy-in'd by the image build. Port values are
// threaded via the /etc/default/bladerunner-watchdog env file (written by
// renderTimeHeal), NOT by substitution into this body.
//
//go:embed scripts/bladerunner-watchdog.sh
var watchdogScript string

// watchdogUnit is the systemd unit for the watchdog.
//
//go:embed scripts/bladerunner-watchdog.service
var watchdogUnit string

// chronyConf is the suspend-tuned chrony.conf written to /etc/chrony/chrony.conf
// in the cloud-init path. makestep 1.0 -1 steps the clock for ANY offset >1s an
// UNLIMITED number of times — the guest-local recovery for a host suspend (no
// paravirt "you were stopped" signal exists).
//
//go:embed scripts/chrony.conf
var chronyConf string

// relayTemplateUnit is the single parameterized systemd unit that supervises all
// four vsock relays. Each channel is a template instance
// (bladerunner-vsock-relay@<name>.service) whose socat argv + optional
// backend-wait port come from /etc/bladerunner/relays/<name>.env. This replaces
// the four near-identical standalone units (ssh/incus/oidc/ntp), collapsing
// their duplicated [Unit]/[Service] boilerplate and Restart policy into one
// definition. See relayChannels for the per-channel args.
//
//go:embed scripts/bladerunner-vsock-relay@.service
var relayTemplateUnit string

// relayChannel is one vsock relay instance: the systemd template instance name,
// the exact socat address pair (word-split by systemd's $RELAY_ARGS expansion
// into socat's argv), and an optional backend TCP port to spin-wait for before
// starting (empty for channels that dial out over vsock rather than proxy a
// local listener).
type relayChannel struct {
	name string // template instance name (ssh/incus/oidc/ntp)
	args string // socat address pair, byte-identical to the old per-channel unit
	wait string // backend TCP port for ExecStartPre spin-wait, or "" for none
}

// relayChannels returns the four vsock relay channels in a fixed order, each
// carrying the exact socat invocation of the standalone unit it replaces:
//
//	ssh   VSOCK-LISTEN:<sshPort>,fork,reuseaddr  TCP:127.0.0.1:22    (wait :22)
//	incus VSOCK-LISTEN:<apiPort>,fork,reuseaddr  TCP:127.0.0.1:8443  (wait :8443)
//	oidc  TCP-LISTEN:<localOIDC>,bind=127.0.0.1,fork,reuseaddr  VSOCK-CONNECT:2:<vsockOIDC>
//	ntp   UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr       VSOCK-CONNECT:2:<vsockNTP>
//
// The ports are threaded from cfg so a non-default port config renders the same
// socat lines the old inline heredocs did.
func relayChannels(cfg *config.Config) []relayChannel {
	return []relayChannel{
		{
			name: "ssh",
			args: fmt.Sprintf("VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:22", cfg.VsockSSHPort),
			wait: "22",
		},
		{
			name: "incus",
			args: fmt.Sprintf("VSOCK-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:8443", cfg.VsockAPIPort),
			wait: "8443",
		},
		{
			name: "oidc",
			args: fmt.Sprintf("TCP-LISTEN:%d,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:%d", cfg.LocalOIDCPort, cfg.VsockOIDCPort),
		},
		{
			name: "ntp",
			args: fmt.Sprintf("UDP4-RECVFROM:123,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:%d", cfg.VsockNTPPort),
		},
	}
}

// relayEnvFile renders the /etc/bladerunner/relays/<name>.env body for one
// channel: the RELAY_ARGS line socat is exec'd with, plus a RELAY_WAIT line only
// when the channel proxies a local backend. systemd reads everything after
// RELAY_ARGS= to end-of-line as the value (spaces included), then $RELAY_ARGS in
// the template's ExecStart word-splits it back into socat's argv.
func relayEnvFile(ch relayChannel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "RELAY_ARGS=%s\n", ch.args)
	if ch.wait != "" {
		fmt.Fprintf(&b, "RELAY_WAIT=%s\n", ch.wait)
	}
	return b.String()
}
