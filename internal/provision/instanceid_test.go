package provision

import (
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// instanceIDOf extracts the meta-data instance-id.
func instanceIDOf(t *testing.T, cfg *config.Config) string {
	t.Helper()
	_, metaData := BuildCloudInit(cfg, "cert")
	for line := range strings.SplitSeq(metaData, "\n") {
		if after, ok := strings.CutPrefix(line, "instance-id: "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("no instance-id in meta-data:\n%s", metaData)
	return ""
}

// cloud-init runs its per-instance modules once per instance-id and then never
// again. The identity must therefore change when the user-data changes, or a
// disk that has booted once keeps whatever it was first given and every later
// edit is silently discarded.
//
// This is not hypothetical. A disk provisioned before the dedicated SSH user
// existed kept its old identity, so the module that creates the user and
// installs its authorized_keys never ran again. Every cloud-init stage still
// reported success, boot still reported ready, and `br shell` failed with
// "Permission denied (publickey)" forever after.
func TestInstanceIDChangesWithTheUserData(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"ssh user", func(c *config.Config) { c.SSHUser = "someone-else" }},
		{"ssh public key", func(c *config.Config) { c.SSHPublicKey = "ssh-ed25519 AAAB rotated@host" }},
		{"hostname", func(c *config.Config) { c.Hostname = "renamed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := instanceIDOf(t, testConfig())

			changed := testConfig()
			tc.mutate(changed)
			after := instanceIDOf(t, changed)

			if before == after {
				t.Errorf("instance-id is %q both before and after changing the %s; "+
					"cloud-init will skip the modules that would apply the change", before, tc.name)
			}
		})
	}
}

// The identity tracks the payload, not the configuration. A field that never
// reaches the user-data must not change it: re-provisioning a guest whose
// user-data is byte-identical would re-run the bootstrap for nothing.
//
// Architecture is the case in point. It is a first-class config field, but
// DefaultAptMirrorURI ignores it and nothing else in the document varies by
// arch, so the rendered payload is arch-independent. A different architecture
// means a different base image and therefore a different disk in any case.
func TestInstanceIDIgnoresConfigurationThatDoesNotReachTheUserData(t *testing.T) {
	arm := testConfig()
	arm.Arch = "arm64"
	amd := testConfig()
	amd.Arch = "amd64"

	armData, _ := BuildCloudInit(arm, "cert")
	amdData, _ := BuildCloudInit(amd, "cert")
	if armData != amdData {
		t.Skip("user-data now varies by architecture; this expectation needs revisiting")
	}

	if instanceIDOf(t, arm) != instanceIDOf(t, amd) {
		t.Error("instance-id differs though the rendered user-data is identical")
	}
}

// The certificate is part of the user-data, so rotating it must also re-provision.
func TestInstanceIDChangesWithTheClientCertificate(t *testing.T) {
	cfg := testConfig()
	_, first := BuildCloudInit(cfg, "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n")
	_, second := BuildCloudInit(cfg, "-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----\n")

	if first == second {
		t.Error("instance-id is unchanged after the client certificate was rotated")
	}
}

// The other half of the contract: an unchanged configuration must produce an
// unchanged identity. Re-provisioning on every boot would re-run the bootstrap
// each time, which is what deriving the identity from the name avoided.
func TestInstanceIDIsStableForAnUnchangedConfiguration(t *testing.T) {
	first := instanceIDOf(t, testConfig())
	for range 5 {
		if got := instanceIDOf(t, testConfig()); got != first {
			t.Fatalf("instance-id is not stable: %q then %q", first, got)
		}
	}
}

// The identity is read by a human diagnosing a guest, and is consumed by
// cloud-init as a single token.
func TestInstanceIDIsWellFormed(t *testing.T) {
	cfg := testConfig()
	id := instanceIDOf(t, cfg)

	if !strings.HasPrefix(id, "bladerunner-"+cfg.Name) {
		t.Errorf("instance-id %q does not identify the guest %q", id, cfg.Name)
	}
	if strings.ContainsAny(id, " \t\n\"'") {
		t.Errorf("instance-id %q contains whitespace or quoting", id)
	}
	const reasonable = 96
	if len(id) > reasonable {
		t.Errorf("instance-id %q is %d characters, longer than %d", id, len(id), reasonable)
	}
}
