package imagebuild

import "fmt"

// Incus initialization performed at BAKE time rather than on every first boot.
//
// Only the STORAGE POOL is created here, via a preseed. `--auto` would also
// create the incusbr0 bridge, and it picks that bridge's subnet by scanning the
// interfaces of the machine it runs on — so baking it would freeze a range
// chosen on a build runner into every user's guest, where it may collide with
// their own network. Running the real bake is what surfaced this: the builder
// had no free subnet at all and the step failed outright.
//
// The storage pool has no such dependency. It is a directory under
// /var/lib/incus and is identical on every machine, so it is exactly the half
// worth paying for once at image release. The network stays a first-boot
// decision, made against the network the guest actually has.
const (
	// incusDaemonPath is the daemon binary. It is NOT on $PATH: the incus
	// package ships no `incusd` in /usr/bin or /usr/sbin, and this path comes
	// from the systemd unit's ExecStart. Invoking it by name silently finds
	// nothing.
	incusDaemonPath = "/usr/libexec/incus/incusd"

	// incusSocketPath is what the daemon creates when it is ready to answer.
	incusSocketPath = "/var/lib/incus/unix.socket"

	// incusStartTimeoutSeconds bounds the wait for that socket. The daemon
	// answers in a second or two; this only exists so a bake cannot hang.
	incusStartTimeoutSeconds = 60

	// incusStoragePreseed creates the default dir-backed pool and points the
	// default profile's root disk at it — what `--auto` does for storage, with
	// no networks section, so no builder-dependent subnet is chosen.
	incusStoragePreseed = `storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  devices:
    root:
      path: /
      pool: default
      type: disk
`

	// incusServerCertGlob is the server identity `incus admin init` generates.
	//
	// It MUST NOT ship in the image. Baking it would give every VM built from
	// this image the same Incus server certificate AND private key, published
	// in a public release — so anyone could impersonate any user's Incus API.
	// Deleting it costs nothing: incusd regenerates a fresh keypair on first
	// boot when it finds none, which is a local keygen rather than a network
	// round trip.
	incusServerCertGlob = "/var/lib/incus/server.crt /var/lib/incus/server.key"
)

// incusInitSteps initialize Incus inside the image being baked.
//
// The daemon is started by hand because a chroot has no systemd to start it,
// and stopped again before the image is sealed. Every step is REQUIRED: unlike
// the web UI, this does not reach a third-party archive, so a failure here is a
// bug in the recipe rather than someone else's outage, and an image that
// silently shipped without it would reintroduce the 2m19s it exists to remove.
func incusInitSteps() []Step {
	return []Step{
		{
			Kind: StepRun,
			Desc: "create the Incus storage pool at bake time",
			// One shell step rather than several: the daemon has to be running
			// for the middle of it and gone by the end, so splitting it would
			// leave a daemon alive between steps with nothing owning it.
			Argv: []string{"/bin/sh", "-c", incusInitScript()},
		},
	}
}

// incusInitScript starts the daemon, initializes, and stops it again.
//
// `set -e` matters: without it a failed init would be followed by a successful
// cleanup and the step would report success having done nothing.
func incusInitScript() string {
	return fmt.Sprintf(`set -e
PRESEED='%[5]s'
test -x %[1]s
%[1]s --group incus >/tmp/incusd-bake.log 2>&1 &
daemon=$!
for i in $(seq 1 %[3]d); do
  [ -S %[2]s ] && break
  sleep 1
done
if [ ! -S %[2]s ]; then
  echo "incusd did not create %[2]s within %[3]ds" >&2
  cat /tmp/incusd-bake.log >&2 || true
  exit 1
fi
printf '%%s' "$PRESEED" | incus admin init --preseed
incus storage list
kill "$daemon" 2>/dev/null || true
wait "$daemon" 2>/dev/null || true
# The server identity must not ship. See incusServerCertGlob.
rm -f %[4]s
rm -f /tmp/incusd-bake.log
`, incusDaemonPath, incusSocketPath, incusStartTimeoutSeconds, incusServerCertGlob, incusStoragePreseed)
}
