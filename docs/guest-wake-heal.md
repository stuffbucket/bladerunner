# Guest-local wake-heal (chrony + watchdog)

When the Mac sleeps, Apple's Virtualization.framework (VZ) pauses the guest's
vCPUs. There is no clean ACPI suspend, no paravirt "you were stopped" signal, and
no host PTP clock. On wake the guest has *no event* telling it it was suspended.
Two things can wedge a previously-healthy VM across a host sleep:

1. **Clock skew** — the guest wall clock can be far behind real time, which breaks
   OIDC JWT `iat`/`exp`/`nbf` validation (token exchange fails).
2. **Stale vsock connectivity** — the socat VSOCK↔TCP relays (`br shell`,
   the Incus API, the OIDC bridge) can be wedged with no SSH banner.

This change provisions a **guest-local backstop** for both, with no dependency on
`br-agent` being enabled and no dependency on the host being reachable. It is the
recovery layer for exactly the case the host-side reconnect path cannot reach
(vsock SSH down).

## What it does

- **chrony replaces `systemd-timesyncd`** in every provisioning path
  (`/etc/chrony/chrony.conf`, canonical copy
  `internal/provision/scripts/chrony.conf`). Tuned with
  `makestep 1.0 -1` so chrony steps the clock for *any* offset >1s an *unlimited*
  number of times — on its first post-resume NTP measurement. timesyncd is
  disabled+masked **only after chrony is verified active** (a transient chrony
  install failure must never strand the guest with zero time sync).
- **The NTP source is the *host clock*, served over vsock** (not the public
  Debian pool). chrony now points at `server 127.0.0.1 iburst prefer minpoll 4
  maxpoll 6`, which a guest-local socat bridge
  (`internal/provision/scripts/bladerunner-vsock-ntp.service`:
  `UDP4-RECVFROM:123,fork` ↔ `VSOCK-CONNECT:2:<VsockNTPPort>`) relays over vsock
  to a host pseudo-NTP (SNTP) responder (`internal/timesource`,
  `cmd/bladerunner/start.go`). The responder stamps each 48-byte reply from the
  **host** wall clock as a stratum-1 source. Consequences: (1) the guest coheres
  to the host clock, *not* UTC — correct even when the host clock is "wrong";
  (2) it works **fully offline** (airplane mode) because vsock needs no IP
  network, gateway, or host internet. The public pool is intentionally **dropped**
  (it requires internet and would sync to UTC). `maxpoll 6` (~64s) re-checks
  tighter than the old global `8` (~256s) so a post-sleep offset is noticed
  sooner. The host responder is started **before** the VM (the vsock reverse
  forwarder has no dial retry), and the watchdog observes + bounces the
  `bladerunner-vsock-ntp` relay alongside the other socat relays.
- **A guest-local watchdog** (`internal/provision/scripts/bladerunner-watchdog.{sh,service}`) is a
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
| cloud-init bootstrap | `internal/provision/cloudinit.go` (`renderTimeHeal`) | `go:embed`s the `internal/provision/scripts/*` files (see `embed.go`) and emits them verbatim |
| image build, virt-customize | `scripts/build-guest-image.sh` | `--copy-in internal/provision/scripts/chrony.conf` + `bladerunner-watchdog.{sh,service}` + `bladerunner-vsock-ntp.service` |
| image build, nbd/chroot | `scripts/build-guest-image.sh` | `install` the same `internal/provision/scripts/*` files (incl. `bladerunner-vsock-ntp.service`) |
| br-agent minimal cloud-init | `internal/provision/cloudinit.go` (`buildMinimalCloudInit`) | **provisions nothing** — relies on the pre-baked image carrying chrony + watchdog |

**Single source of truth:** `internal/provision/scripts/chrony.conf`,
`internal/provision/scripts/bladerunner-watchdog.{sh,service}`, and
`internal/provision/scripts/bladerunner-vsock-ntp.service` are the ONE canonical
copy. The cloud-init Go path `go:embed`s them (see `internal/provision/embed.go`)
and the image build `--copy-in`s the same files, so there is no second copy to
keep in sync. Port values are threaded via a templated
`/etc/default/bladerunner-watchdog` env file, not by string substitution into the
script body; the NTP bridge's `VsockNTPPort` is the one templated value, and its
default *is* the `18557` baked into the checked-in unit file (`vsockNTPUnit`
re-templates that literal at render time).

CI exercises the cloud-init path (`internal/provision/cloudinit_test.go`); the
two image-build arms `--copy-in`/`install` the identical embedded files, so they
cannot drift from the cloud-init emission.

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
- **CONFIRMED (unit-tested):** the SNTP reply byte layout (mode=4, stratum=1, VN
  echoed, client-transmit echoed into origin, host-now timestamps); cloud-init
  emits the `bladerunner-vsock-ntp.service` unit + the host-pointed
  `server 127.0.0.1` chrony line + the public pool dropped, all `go:embed`'d from
  `internal/provision/scripts/`.
- **HYPOTHESIS (cannot be unit-tested here):** (1) socat
  `UDP4-RECVFROM:123,fork` ↔ `VSOCK-CONNECT` single request/response NTP
  semantics end-to-end on this stack (the documented "ntpd-like" use case, but
  unverified here); (2) chrony accepting a `127.0.0.1` stratum-1 source as
  authority (not flagging falseticker/unsynchronised) — needs `chronyc
  sources`/`tracking` on a real guest; (3) **the whole point** — post host-sleep,
  `makestep` stepping the guest to host time within a poll *in airplane mode*.

## Validation gap

End-to-end sleep/wake recovery **cannot be unit-tested**. Whether `makestep`
fires fast enough to fix OIDC after a multi-day sleep, and whether the forwarders
self-heal, requires a **real Mac sleep/wake**. The host-clock-over-vsock path
adds its own gap: whether the socat UDP↔vsock bridge actually delivers the
host's stratum-1 reply to chrony, whether chrony accepts `127.0.0.1` as
authority, and whether the guest steps to host time within a poll **in airplane
mode** all require a **real Mac sleep/resume + airplane-mode run** and cannot be
exercised in CI. CI asserts only that the provisioning strings are emitted. On a
live guest, confirm the source is accepted with `chronyc sources` / `chronyc
tracking` (refid should show the host source, not a pool). The journal logs
(`logger -t bladerunner-watchdog`) are what will turn the
clock-vs-connectivity inference into a measurement on the next real wedge:

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
