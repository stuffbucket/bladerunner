# Guest-local wake-heal (chrony + watchdog)

When the Mac sleeps, Apple's Virtualization.framework (VZ) pauses the guest's
vCPUs. There is no clean ACPI suspend, no paravirt "you were stopped" signal, and
no host PTP clock. On wake the guest has *no event* telling it it was suspended.
Two things can wedge a previously-healthy VM across a host sleep:

1. **Clock skew** — the guest wall clock can be far behind real time, which breaks
   OIDC JWT `iat`/`exp`/`nbf` validation (token exchange fails).
2. **Stale vsock connectivity** — the socat VSOCK↔TCP relays (`runner shell`,
   the Incus API, the OIDC bridge) can be wedged with no SSH banner.

This change provisions a **guest-local backstop** for both, with no dependency on
`br-agent` being enabled and no dependency on the host being reachable. It is the
recovery layer for exactly the case the host-side reconnect path cannot reach
(vsock SSH down).

## What it does

- **chrony replaces `systemd-timesyncd`** in every provisioning path
  (`/etc/chrony/chrony.conf`, canonical copy `scripts/chrony.conf`). Tuned with
  `makestep 1.0 -1` so chrony steps the clock for *any* offset >1s an *unlimited*
  number of times — on its first post-resume NTP measurement. timesyncd is
  disabled+masked **only after chrony is verified active** (a transient chrony
  install failure must never strand the guest with zero time sync).
- **A guest-local watchdog** (`scripts/bladerunner-watchdog.{sh,service}`) is a
  `Type=simple` systemd service running an in-process loop. Every 60s it **logs**
  the chrony clock offset, the RTC-vs-realtime delta, and per-forwarder health to
  the journal — *even when healthy* — and heals conservatively:
  `chronyc makestep` when unsynchronised; bounce the stateless socat relays
  (incus, oidc) when the relay itself is inactive; bounce vsock-ssh **last** and
  only when local sshd `:22` is up but the relay is dead. It **never**
  blanket-restarts `systemd-networkd` (that would tear down Incus container
  bridges).

## Provisioning paths (all kept in sync)

The same chrony swap + watchdog is applied identically across every path that
provisions a guest; missing one would silently leave that path on timesyncd with
no backstop:

| Path | Where | chrony + watchdog source |
| --- | --- | --- |
| cloud-init bootstrap | `internal/provision/cloudinit.go` (`renderTimeHeal`) | embeds a byte-for-byte copy of the watchdog logic + the `scripts/*` content |
| image build, virt-customize | `scripts/build-guest-image.sh` | `--copy-in scripts/chrony.conf` + `scripts/bladerunner-watchdog.{sh,service}` |
| image build, nbd/chroot | `scripts/build-guest-image.sh` | `install` the same `scripts/*` files |
| br-agent minimal cloud-init | `internal/provision/cloudinit.go` (`buildMinimalCloudInit`) | **provisions nothing** — relies on the pre-baked image carrying chrony + watchdog |

**Single source of truth:** `scripts/chrony.conf` and
`scripts/bladerunner-watchdog.{sh,service}` are canonical. The cloud-init Go path
embeds a byte-for-byte copy of the executable logic (port values are threaded via
a templated `/etc/default/bladerunner-watchdog` env file, not by string
substitution into the script body). If you change one, change all of them.

CI asserts the cloud-init path only (`internal/provision/cloudinit_test.go`); the
two image-build arms have **no automated guard** — they must be checked by review.

## Confirmed vs. hypothesis (read before "fixing" this)

The knobs here are chosen from first principles, not measurement. State this
honestly:

- **CONFIRMED:** the guest cannot self-detect host suspend by comparing its own
  monotonic vs. wall clock. On ARM64 both `CLOCK_MONOTONIC` and `CLOCK_REALTIME`
  are driven by the same free-running counter (`arch_sys_counter`; no kvm-clock,
  no `/dev/ptp`). When VZ pauses the vCPUs both freeze and resume together, so a
  Δmonotonic-vs-Δrealtime check yields ~0 and detects nothing. The only
  guest-local skew detector is an **external** reference: chrony's NTP offset.
- **CONFIRMED (more certain symptom):** stale vsock connectivity ("no SSH
  banner") is the more reliably-observed wedge.
- **PLAUSIBLE but UNCONFIRMED:** post-sleep clock skew breaking OIDC JWTs. The
  wedged VM in the original report was restarted before its clock could be
  measured, so this remains a hypothesis. The watchdog's per-cycle clock log is
  the instrument that will confirm or refute it on the next wedge.
- **UNKNOWN:** whether the VZ emulated RTC advances during host sleep, or freezes
  with the vCPUs (in which case it is no better than the frozen wall clock). The
  watchdog only **logs** `rtc_delta`; **no heal path depends on the RTC** until a
  real Mac-sleep spike proves it tracks real time.
- **Hypothesis-driven:** `makestep 1.0 -1` and the offset magnitude are guesses.

## Validation gap

End-to-end sleep/wake recovery **cannot be unit-tested**. Whether `makestep`
fires fast enough to fix OIDC after a multi-day sleep, and whether the forwarders
self-heal, requires a **real Mac sleep/wake**. CI asserts only that the
provisioning strings are emitted. The journal logs (`logger -t
bladerunner-watchdog`) are what will turn the clock-vs-connectivity inference into
a measurement on the next real wedge:

```bash
runner shell
journalctl -t bladerunner-watchdog --since '-1h'
# look for: clock sys_offset=… leap=… rtc_delta=…s ; listen ssh22=… ; heal: …
```

## Coordination (followup)

The host-side reconnect path (`cmd/bladerunner/reconnect.go`) restarts
`systemd-timesyncd` over SSH. Once timesyncd is masked, that restart no-ops on a
masked unit. That file does **not** exist on `main` yet (it is introduced by the
unmerged menubar/wake-handler work), so the rename — drop `systemd-timesyncd`,
run `chronyc makestep` instead — must land with whichever PR carries
`reconnect.go`. Flagged here so the two changes do not contradict.
