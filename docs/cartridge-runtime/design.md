# Design: standalone cartridge runtime

> The architecture of record for the cartridge runtime. It supersedes the
> deferred DiskArbitration recommendation in `docs/instance-floppies/prd.md` §8.D.
>
> This document is the *why* and the *shape*. The practical guide to driving it
> is [usage.md](usage.md); what changed for an existing script is
> [behaviour-changes.md](behaviour-changes.md); known gaps are
> [status.md](status.md). Which package owns which operation is AGENTS.md §3.

**One-line summary:** a cartridge is a DMG holding one entire VM; a minimal holder
process owns that VM and outlives the CLI; bladerunner becomes a manager of many
holders that notices cartridges being inserted and drains them safely on eject.

---

## 1. Goals

1. **Standalone holder.** The VM is owned by a minimal wrapper process that runs
   whether or not the rest of bladerunner is up. The CLI and the menubar can come
   and go without killing a running VM.
2. **Multi-instance.** bladerunner manages N holders concurrently.
3. **The cartridge is the unit.** One DMG contains everything for that instance
   except bladerunner itself. Transportable (AirDrop, copy) and simple to run.
4. **Insertion is detected.** Mounting a cartridge DMG is noticed, and the user is
   offered a boot — *when something is watching*. Detection needs a foreground
   `br watch` or the menubar, and the menubar is installed only by an explicit
   `br menubar install`. Nothing detects out of the box: a fresh install that
   double-clicks a cartridge DMG gets a mounted volume in Finder and no offer.
5. **Ejection is orderly.** Requesting an unmount drains the VM first — no
   corruption.

Relationship to *instance floppies* (`docs/instance-floppies/`): that parked design
carries a single **Incus instance** as a DMG. This one carries a whole **machine**.
They coexist; §9 of that PRD already assumes it.

---

## 2. Process model

Three roles, two host processes per running instance.

- **Manager** — the `br` CLI verbs and the menubar. Short-lived, or a long-lived
  singleton owning an `NSApp` run loop. **Never owns a VM.**
- **Holder** — exactly one process per running instance. Owns the
  `*vz.VirtualMachine`, the vsock forwarders, the cartridge mount, the control
  socket, and the unmount-approval registration. Detached from whatever spawned it.
- **Guest agent** — unchanged.

Every ordinary verb that needs a VM — `br start`, `br up`, `br boot <disk>`,
`br boot <cartridge>`, `br restore`, `br upgrade`, and the auto-start behind a
verb that needs a running VM — spawns a holder and *attaches* to it: the boot
board and the running summary are rendered from the console log, the boot-stage
file and the registry entry, and the command returns with the VM still running.

`--gui` is the one exception and stays in the foreground, because
`vz.StartGraphicApplication` owns the calling process's main thread. A GUI VM
therefore cannot run under a holder.

### Decision: re-exec `br`, not a new `cmd/br-vmd` binary

The holder is a hidden cobra subcommand, `br vmd --state-dir <dir> [--spec <file>]`,
spawned by re-exec of `os.Executable()`.

The deciding factor is that **the VZ entitlement is per-binary**. A second binary
would have to be codesigned with `vz.entitlements` in the goreleaser build hook,
`make sign`, `make build-release`, and the README's manual-download path; it would
have to be embedded in `Bladerunner.app` as nested signed code and included in
notarization; and sibling-binary path resolution breaks under `go run`, under brew
relocation, inside the `.app`, and in the dev worktree. `os.Executable()` re-exec has
none of those failure modes.

Binary size is the only thing a separate binary buys, and it is not a goal here.

**Mitigation for "minimal":** all holder logic lands in `internal/vmhost`, which
imports no cobra, no systray, and no Cocoa. `cmd/bladerunner/vmd.go` is a thin
shim. If a separate binary is ever justified, `cmd/br-vmd/main.go` becomes ~30
lines — the expensive part (the package split) is already paid.

The whole `vmhost.Spec` travels to the holder as a JSON hand-off file, which is
what lets flags like `--persist` and `--private-mount` reach a cartridge the
holder opens itself.

### Surviving manager exit

Spawn with `SysProcAttr{Setsid: true}`, stdio redirected to the holder log, then
`Process.Release()`. `setsid` detaches the controlling terminal so a terminal
close never reaches the holder.

The holder handles **SIGTERM only** (it has no terminal) and treats it as *orderly
eject*, routing into the drain rather than a fast cancel.

Ownership tokens, in priority order: the bound `<stateDir>/control.sock` (already
the ownership primitive — `NewListener` dials-then-binds), then the registry
entry's PID. The plain dial-then-bind has a TOCTOU hole (two racing starts can
each fail the dial, and the second unlinks the first's live socket), so the holder
takes a lock file before the dial/bind dance.

**Version skew becomes permanent.** Once holders outlive the CLI, `brew upgrade`
replaces `br` while old holders keep running old code. `ProtocolVersion`
negotiation already exists on both sides; the rule is that a newer manager must
*degrade gracefully* against an older holder rather than hard-erroring — otherwise
a new `br` cannot eject an old holder and strands a mounted cartridge.

---

## 3. Instance discovery

`internal/instance` owns a registry at `<stateDir>/instances/<name>.json`, written
atomically by the holder immediately after it binds the control socket, and
removed on clean exit.

Entry: `Name`, `Kind` (`flat` | `disk` | `cartridge`), `StateDir`, `SourcePath`,
`WorkingCopy`, `DevNode`, `Mountpoint`, `UnmountProtection`, `PID`, `Ports`,
`ProtocolVersion`, `BinaryVersion`, `StartedAt`, `GUI`.

`List()` = registry ∪ a legacy scan of the old layout (so existing installs keep
working), reconciled by a liveness ladder rather than a bare ping — a wedged
holder that answers nothing still owns its slot's disk image. Entries with a dead
socket and a gone PID are pruned.

The registry is **required**, not a convenience: once cartridges mount under
`/Volumes` (§5), the `<state>/mnt/*` scan no longer finds them.

`--instance` selects the target for every verb that acts on one VM; a verb that
does *not* act on a selected instance **refuses** the flag rather than silently
ignoring it (see [behaviour-changes.md](behaviour-changes.md) §4).

---

## 4. Per-instance ports

`internal/portalloc.Reserve(name, preferred)` returns a **bound `net.Listener`**,
not a port number — returning a number and re-binding later is a TOCTOU race where
another process steals the port in between.

Policy: the **flat default instance keeps 6022 / 18443 / 18444 / 15556 / 15557** so
existing muscle memory, docs, and hand-written ssh configs keep working. Every
*additional* instance takes ephemeral ports.

Two traps, both real:

- `OIDCIssuerURL` used to be built by `fmt.Sprintf` over the **constant**, not the
  config field. Reassigning `LocalOIDCPort` without re-deriving the issuer URL
  breaks OIDC silently — and only at login time, long after boot looks successful.
  `config.AssignPorts` now re-derives it.
- `incusClientFromControl` reads the port live but takes the client certificate
  from `config.Default("")`.

Vsock ports stay constant — each VM has its own `VirtioSocketDevice`, so they are
already per-VM namespaced.

SSH config becomes an aggregator: `~/.config/bladerunner/ssh/config` keeps the
legacy `Host bladerunner` block and adds `Include config.d/*`; each instance writes
its own `config.d/<name>` with `Host bladerunner-<name>`. Per-instance files no
longer clobber each other, and both the fragment and the aggregator are published
atomically.

---

## 5. Mount policy: an inversion

The original cartridge mount attached `-nobrowse` at `<state>/mnt/<name>`,
deliberately invisible in Finder.

Goals 4 and 5 need the opposite, because **"eject the cartridge" is the gesture that
triggers orderly spin-down**. The default is to attach browsable, let macOS place
the volume at `/Volumes/bladerunner-<name>`, and capture the real mountpoint *and
BSD device node* by parsing `hdiutil attach -plist`.

The policy is a value, `cartridge.MountPolicy`, whose zero value resolves to
`MountBrowsable`. `MountPrivate` keeps the dictated-mountpoint behaviour: it is
what `br disk pack` uses unconditionally, and what a boot opts into with
`--private-mount`. Pack being pinned to the private policy is what removes the
pack-vs-boot mountpoint collision.

Reading the mountpoint back instead of dictating it also handles name-collision
suffixing (`bladerunner-demo 1`) for free.

Capturing the dev node is the blocking prerequisite for §6: DiskArbitration
addresses disks by BSD name.

A cartridge is named after **its own file**, not after the disk it was packed
from, at both ends — `br disk pack` seeds the name from its output path and
`br boot` derives it the same way. That is what stops two cartridges packed from
one base disk from claiming a single `/Volumes` path.

Mutual exclusion is keyed on the **image**, not on the mountpoint: macOS chooses
the mountpoint, so nothing derived from one can see a second Finder mount of the
same file under a different volume name. The claim is an `flock(2)` on a hidden
sibling of the working copy, holding two identities at once — the
symlink-resolved path (the only identity an image that does not exist yet can
have, which is where every `.dmg` boot starts) and the device/inode once the file
exists (the only identity two hard links share). `flock` is used rather than an
`O_EXCL` marker because the kernel drops it however the holder died, so a crash
leaves a stale lock *file* (harmless, reused in place) and never a stale *lock*.

---

## 6. Unmount veto lives in the holder

The premise that DiskArbitration requires a `CFRunLoop` — the reason §8.D of the
floppies PRD deferred it — is only half true. `DASessionSetDispatchQueue` schedules
callbacks on a serial dispatch queue with no run loop and no `NSApplication`.

That settles placement: the veto belongs in the **holder**, which is headless — and
which, in GUI mode, has already surrendered its main thread to
`vz.StartGraphicApplication` and therefore *cannot* host a main-thread run loop.

`internal/diskarb` follows the established `_darwin.go` / `_other.go` split
(`ErrUnsupported` off-darwin, so `GOOS=linux` CI stays green).

Flow on unmount approval:

1. Callback fires on the DA queue for the holder's own disk. If the VM already
   reached `Stopped` and teardown is underway → approve (return `NULL`).
2. Otherwise return `DADissenterCreate(..., kDAReturnBusy, "bladerunner is shutting
   down the VM on this cartridge")` — Finder surfaces that string — and, guarded by
   `sync.Once`, kick off the drain on a background goroutine. **The callback returns
   immediately; it never blocks for the drain budget.**
3. Drain completion re-enters normal teardown: release `root.img`, unregister the
   approval callback, unmount, remove the registry entry, exit. The user's second
   eject click (or the self-unmount) succeeds.
4. Progress is surfaced by `internal/bootstage`'s drain/eject stages plus a
   menubar notification.

The veto arms only when **all** of: kind is cartridge, a cartridge is attached,
the dev node reduces to a bare BSD name, the DiskArbitration session opens, and
the watch registers. Every failure **fails open** with a warning, so the VM still
runs — and the reason is published on the registry entry, so `br instances` and
`br status` report it rather than burying it in a log.

**cgo cancellation order is load-bearing.** `cgo.Handle` lifetime versus
`DAUnregisterCallback` ordering is the classic crash in this design.
`internal/diskarb` cancels in a fixed order — mark canceled,
`DAUnregisterCallback`, drain the serial queue with a `dispatch_sync` barrier, and
only then `Handle.Delete()` — with the session lock released before the barrier so
a callback canceling itself cannot deadlock.

### Honest limitation

`DADissenter` is **advisory** — see [usage.md](usage.md) *Limitations*. This is
precisely why the crash-consistency work (a real wait-for-stopped in `Stop`,
explicit VZ cache/sync mode, fsync before detach) is not optional.

---

## 7. Mount detection lives in the manager

Same `internal/diskarb`, `WatchAppeared` on the menubar's dispatch queue, with a
headless `br watch` mode for users without the menubar.

Per appearing volume, `decideForVolume` is pure and runs in this order: a cheap
filter on the `bladerunner-` volume-name prefix (this callback fires for every USB
stick on the machine); a check that no instance already *holds* the volume,
because a booted cartridge's own mount looks exactly like a fresh insertion; then
the authoritative classification — `cartridge.Detect` reading the volume itself.
A cartridge that is real but unbootable is reported **with the reason**, not
silently skipped. For a read-only `.dmg` mount, the backing image path is
recovered from `hdiutil info -plist` keyed on the dev node.

On accept, the manager unmounts the read-only view and spawns a holder with the
*source* path; the holder does convert → attach → boot.

**TCC caveat:** AirDropped cartridges land in `~/Downloads`, and the menubar runs
from a LaunchAgent with no user-initiated open, so the watcher can find a volume
it is not allowed to read. `Bladerunner.app`'s generated `Info.plist` carries
`NSDownloadsFolderUsageDescription` and `NSRemovableVolumesUsageDescription`, and
a permission failure is additionally *reported* to the user with its reason
rather than passed over silently.
