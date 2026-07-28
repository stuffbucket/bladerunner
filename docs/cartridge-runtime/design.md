# Design: standalone cartridge runtime

> **Status (2026-07-26):** In progress on `refactor/cartridge-standalone-runtime`.
> W1–W8 have landed; W9 has landed in part (the CLI, plus cartridge detection in
> the menubar) and W10 is outstanding. Per-workstream detail is in §9; the
> practical guide to what exists today is [usage.md](usage.md).
> This document is the architecture of record for the refactor; it supersedes the
> deferred DiskArbitration recommendation in `docs/instance-floppies/prd.md` §8.D.

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
   offered a boot.
5. **Ejection is orderly.** Requesting an unmount drains the VM first — no
   corruption.

Relationship to *instance floppies* (`docs/instance-floppies/`): that parked design
carries a single **Incus instance** as a DMG. This one carries a whole **machine**.
They coexist; §9 of that PRD already assumes it.

---

## 2. What already existed

The starting point was better than expected — roughly half the goal was built by
PR #71 (bootable disks) and PR #72 (AirDrop-able cartridges):

| Requirement | Status at start | Evidence |
|---|---|---|
| 1. Standalone holder | partial | `startVMDetachedAndWait` already forks `br start` with `Setsid` + `Process.Release` (`vmgate.go`, `vmgate_unix.go`) — but only for the flat default slot. (Now delivered: see W5b) |
| 2. Multi-instance | partial | Addressing is already per-directory: `config.Default(baseDir)`, `control.SocketPath(stateDir)`, and `resolveEjectSlot` already enumerates three slot families |
| 3. Cartridge DMG | **mostly done** | `br disk pack --ship` builds a UDZO `.dmg` with `disk.json` / `root.img` / `state/` / `share/`; `br boot` converts to a writable `.sparseimage` so the shipped artifact stays pristine |
| 4. Mount detection | **missing** | No DiskArbitration, NSWorkspace, or FSEvents code anywhere in the tree |
| 5. Orderly eject | partial | `Runner.Eject` is already correct: ACPI `RequestStop` then a genuine wait for `VirtualMachineStateStopped` before releasing `root.img` |

So this is a **refactor, not a rewrite**: lift cartridge and VM-lifecycle semantics
out of `package main` into importable packages, make ports per-instance, add one
cgo package, and add a holder mode plus a registry.

### The load-bearing bug found on the way

`br stop` and Ctrl+C did *not* use the correct `Eject` path. `requestStopVM` looped
`RequestStop` three times with a fixed `time.Sleep(2s)`, never checked `vm.State()`,
and then called the destructive `vz.Stop()` — a hard power cut after ~6 seconds.
It also closed the vsock forwarders *before* asking the guest to power off. Since
the entire cartridge premise is "this DMG is always in a consistent cold-boot
state", fixing this is sequenced first and is not optional.

---

## 3. Process model

Three roles, two host processes per running instance.

- **Manager** — the `br` CLI verbs and the menubar. Short-lived, or a long-lived
  singleton owning an `NSApp` run loop. **Never owns a VM.**
- **Holder** — exactly one process per running instance. Owns the
  `*vz.VirtualMachine`, the vsock forwarders, the cartridge mount, the control
  socket, and the unmount-approval registration. Detached from whatever spawned it.
- **Guest agent** — unchanged.

### Decision: re-exec `br`, not a new `cmd/br-vmd` binary

The holder is a hidden cobra subcommand, `br vmd --state-dir <dir> [--cartridge <path>]`,
spawned by re-exec of `os.Executable()`.

The deciding factor is that **the VZ entitlement is per-binary**. A second binary
would have to be codesigned with `vz.entitlements` in the goreleaser build hook,
`make sign`, `make build-release`, and the README's manual-download path; it would
have to be embedded in `Bladerunner.app` as nested signed code and included in
notarization; and sibling-binary path resolution breaks under `go run`, under brew
relocation, inside the `.app`, and in the dev worktree. `os.Executable()` re-exec has
none of those failure modes and is already the pattern used by
`startVMDetachedAndWait` and `launchDetached`.

Binary size is the only thing a separate binary buys, and it is not a goal here.

**Mitigation for "minimal":** all holder logic lands in `internal/vmhost`, which
imports no cobra, no systray, and no Cocoa. `cmd/bladerunner/vmd.go` is a thin
shim. If a separate binary is ever justified, `cmd/br-vmd/main.go` becomes ~30
lines — the expensive part (the package split) is already paid.

### Surviving manager exit

Spawn with `SysProcAttr{Setsid: true}`, stdio redirected to `<stateDir>/vmd.log`,
then `Process.Release()` — the existing `detachProcess` pattern. `setsid` detaches
the controlling terminal so a terminal close never reaches the holder.

The holder handles **SIGTERM only** (it has no terminal) and treats it as *orderly
eject*, routing into the drain rather than a fast cancel.

Ownership tokens, in priority order: the bound `<stateDir>/control.sock` (already
the ownership primitive — `NewListener` dials-then-binds), then the registry
entry's PID. The existing dial-then-bind has a TOCTOU hole (two racing starts can
each fail the dial, and the second unlinks the first's live socket), so the holder
takes an `O_EXCL` lock file before the dial/bind dance.

**Version skew becomes permanent.** Once holders outlive the CLI, `brew upgrade`
replaces `br` while old holders keep running old code. `ProtocolVersion`
negotiation already exists on both sides; the rule is that a newer manager must
*degrade gracefully* against an older holder rather than hard-erroring — otherwise
a new `br` cannot eject an old holder and strands a mounted cartridge.

---

## 4. Instance discovery

`internal/instance` owns a registry at `<stateDir>/instances/<name>.json`, written
atomically (temp + fsync + rename, copying `internal/bootstage`'s proven pattern) by
the holder immediately after it binds the control socket, and removed on clean exit.

Entry: `Name`, `Kind` (`flat` | `disk` | `cartridge`), `StateDir`, `SourcePath`,
`WorkingCopy`, `DevNode`, `Mountpoint`, `PID`, `Ports`, `ProtocolVersion`,
`BinaryVersion`, `StartedAt`, `GUI`.

`List()` = registry ∪ a legacy scan of the old layout (so existing installs keep
working), reconciled by dialing each `control.sock`; entries with a dead socket and
a gone PID are pruned. This is `resolveEjectSlot` generalized.

The registry is **required**, not a convenience: once cartridges mount under
`/Volumes` (§6), the `<state>/mnt/*` scan no longer finds them.

---

## 5. Per-instance ports

`internal/portalloc.Reserve(name, preferred)` returns a **bound `net.Listener`**,
not a port number — returning a number and re-binding later is a TOCTOU race where
another process steals the port in between.

Policy: the **flat default instance keeps 6022 / 18443 / 18444 / 15556 / 15557** so
existing muscle memory, docs, and hand-written ssh configs keep working. Every
*additional* instance takes ephemeral ports.

Two traps, both real:

- `OIDCIssuerURL` is built by `fmt.Sprintf` over the **constant**, not the config
  field. Reassigning `LocalOIDCPort` without re-deriving the issuer URL breaks OIDC
  silently — and only at login time, long after boot looks successful.
- `incusClientFromControl` reads the port live but takes the client certificate
  from `config.Default("")`. That is already wrong for slot boots today.

Vsock ports stay constant — each VM has its own `VirtioSocketDevice`, so they are
already per-VM namespaced.

SSH config becomes an aggregator: `~/.config/bladerunner/ssh/config` keeps the
legacy `Host bladerunner` block and adds `Include config.d/*`; each instance writes
its own `config.d/<name>` with `Host bladerunner-<name>`. Writes stay `O_TRUNC` but
are now per-instance files, so they no longer clobber each other.

---

## 6. Mount policy: an inversion

Today cartridges attach `-nobrowse` at `<state>/mnt/<name>`, deliberately invisible
in Finder.

Goals 4 and 5 need the opposite, because **"eject the cartridge" is the gesture that
triggers orderly spin-down**. New default: attach browsable, let macOS place the
volume at `/Volumes/bladerunner-<name>`, and capture the real mountpoint *and BSD
device node* by parsing `hdiutil attach -plist`. A `--private-mount` flag retains
today's behaviour for CI, `scripts/smoke-cartridge.sh`, and headless use.

*As landed:* the policy is a value, `cartridge.MountPolicy`, whose zero value
resolves to `MountBrowsable`; `MountPrivate` keeps the old dictated-mountpoint
behaviour and is what `br disk pack` (and `cartridge.Attach`) use. It is not
surfaced as a `--private-mount` CLI flag — no boot path needed one, and pack
being pinned to the private policy is what removes the pack-vs-boot mountpoint
collision listed in §10.

Reading the mountpoint back instead of dictating it also handles name-collision
suffixing (`bladerunner-demo 1`) for free.

Capturing the dev node is the blocking prerequisite for everything below:
DiskArbitration addresses disks by BSD name, and `internal/cartridge` currently goes
out of its way *not* to learn it.

---

## 7. Unmount veto lives in the holder

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
4. Progress is surfaced by extending `internal/bootstage` with drain/eject stages
   plus a menubar notification.

### Honest limitation

`DADissenter` is **advisory**. Finder's "Force Eject", `diskutil unmount force`, and
`DADiskUnmount(kDADiskUnmountOptionForce)` all bypass registered dissenters, and a
direct `umount(2)` never consults DiskArbitration at all. The veto narrows the
corruption window; it does not close it. This is precisely why the crash-consistency
work — a real wait-for-stopped in `Stop`, explicit VZ cache/sync mode, fsync before
detach — is sequenced first and is not optional.

---

## 8. Mount detection lives in the manager

Same `internal/diskarb`, `WatchAppeared` on the menubar's dispatch queue, with a
headless `br watch` mode for users without the menubar.

Per appearing volume: cheap filter on the `bladerunner-` volume-name prefix, then
the authoritative check — a parseable `disk.json` at the mount root. For a read-only
`.dmg` mount, recover the backing image path from `hdiutil info -plist` keyed on the
dev node. Then prompt via the existing notifier machinery. On accept, the manager
unmounts the read-only view and spawns a holder with the *source* path; the holder
does convert → attach → boot exactly as today.

**TCC caveat:** AirDropped cartridges land in `~/Downloads`, and the menubar runs
from a LaunchAgent with no user-initiated open. `NSDownloadsFolderUsageDescription`
and `NSRemovableVolumesUsageDescription` need to be in `Info.plist` or detection may
silently see nothing.

---

## 9. Workstreams

| ID | Title | Depends on | Status |
|---|---|---|---|
| W1 | Orderly drain on every shutdown path | — | **done** — `vm.drainGuest` + `StopOutcome`; `br stop` now carries a drain budget and waits for `Stopped`, forcing only on expiry |
| W2 | Lift cartridge semantics; capture dev node; format version | — | **done** — `internal/cartridge` owns open/verify/detect; `Mount.DevNode`, `cartridge.json`, `FormatVersion` |
| W3 | Per-instance ports, derived OIDC issuer, per-instance ssh config | — | **done** — `internal/portalloc`, `config.AssignPorts` re-derives `OIDCIssuerURL`, `ssh/config.d/<name>` fragments |
| W4 | Extract VM lifecycle into `internal/vmhost` | W1, W2, W3 | **done** — `Spec`/`Host`/`Observer`, ordered steps with reverse teardown |
| W5 | `br vmd` holder mode + instance registry | W4 | **done** — hidden `br vmd`, `internal/instance` registry, `spawnHolder` detached launcher |
| W5b | Holder on the ordinary paths | W5 | **done** — `br start`, `br up`, `br boot <disk>`, `br boot <cartridge>`, `br restore`, `br upgrade` and the auto-start behind a verb that needs a VM all spawn a holder and ATTACH to it (`holderattach.go`): the boot board and the running summary are rendered from the console log, the boot-stage file and the registry entry, and the command returns with the VM still running. `--gui` is the one exception and stays in the foreground, because `vz.StartGraphicApplication` owns the calling process's main thread. `startVMDetachedAndWait` no longer forks `br start`; there is one spawn design. The whole `vmhost.Spec` travels to the holder as a JSON hand-off file (`br vmd --spec`), which is what lets `--persist` and `--private-mount` reach a cartridge the holder opens itself |
| W6 | `internal/diskarb` cgo bridge | — | **done** — dispatch-queue session, appear/disappear/unmount-approval watches, `_other.go` stub |
| W7 | Unmount veto in the holder + browsable mounts | W5, W6 | **done** — `StepUnmountVeto` + `decideUnmount` + background drain; `cartridge.MountPolicy` with `MountBrowsable` as the default and the real mountpoint read back from `hdiutil attach -plist`. `br disk pack` stays on `MountPrivate`, which is what keeps pack and boot off one mountpoint. No CLI flag exposes the private policy for a boot |
| W8 | Mount detection and boot prompt in the manager | W5, W6 | **done** — `decideForVolume` (pure, name-filter → held-check → `cartridge.Detect`), `br watch` (`--yes`/`--auto`, `--once`, `--json`) and the menubar prompt; accept unmounts the read-only view and spawns a holder on the source file. The TCC risk is handled by *reporting* a permission failure, not by `Info.plist` keys — see below |
| W9 | Instance-aware CLI and menubar | W3, W5 | **partial** — CLI done (`--instance`, `br instances`, instance-aware `status`/`stop`/`eject`/`shell`/`ssh`/`config`/incus targeting), and the menubar now offers detected cartridges. The rest of the menubar still assumes a single VM |
| W10 | Cartridge self-containment and persistence | W2, W7 | **outstanding** — beyond the `cartridge.json` metadata W2 added |

**Natural cut line:** W1 + W2 + W4 + W5 alone deliver a detached, orderly-shutdown
cartridge holder with no cgo at all. W6/W7/W8 (DiskArbitration) is what buys goals
4 and 5; cutting it leaves the product where PR #72 left it.

All five goals in §1 are now met end to end. What is left is polish and hardening
rather than architecture: the menubar's remaining single-VM assumptions (W9),
cartridge self-containment (W10), the TCC entitlement keys §8 calls for (today a
permission failure is reported to the user rather than pre-empted), and the
unmitigated risks below.

---

## 10. Risks

Annotated with what happened to each one. A risk that was retired is kept, not
deleted: the reasoning is why the mitigation exists.

- **W4 is the highest-regression item.** Lifting `runStart` (285 lines) is a wide,
  mechanical refactor of the most important and least-directly-tested function in
  the tree. Budget for it landing in two passes.
  *Retired.* It landed as `internal/vmhost`, with the lifecycle expressed as an
  ordered list of named steps so that "teardown is the exact reverse of startup,
  skips steps that never started, and is idempotent" is unit-tested without a VM.
  The residual regression surface is the front ends (`br start`, `br boot`) that
  used to own the code, not the sequence itself.
- **Advisory-only veto** (§7) — mitigated by, not replaced by, crash consistency.
  *Live, and now user-visible.* The veto is in (`StepUnmountVeto`), and so is the
  crash-consistency work it leans on: `Stop`/`Eject` genuinely wait for
  `VirtualMachineStateStopped` before the VMM is released. Force-eject still
  wins, and `docs/cartridge-runtime/usage.md` says so in as many words.
- **cgo + DiskArbitration is new surface** in an otherwise pure-Go + hdiutil-exec
  repo. `cgo.Handle` lifetime vs `DAUnregisterCallback` ordering is the classic bug.
  *Addressed, not eliminated.* `internal/diskarb` cancels in a fixed order — mark
  canceled, `DAUnregisterCallback`, drain the serial queue with a
  `dispatch_sync` barrier, and only then `Handle.Delete()` — with the session
  lock released before the barrier so a callback canceling itself cannot
  deadlock. This remains the part of the tree where a mistake is a crash rather
  than an error.
- **Browsable mounts invert a documented decision** and expose users to accidental
  Finder ejects — each of which now costs a full VM shutdown.
  *Realized, deliberately.* `MountBrowsable` is the default for a boot and there
  is no CLI flag to opt out; `MountPrivate` survives for `br disk pack` and for
  callers that need a deterministic mountpoint. The accidental-eject cost is
  exactly what the veto in §7 exists to convert from "corruption" into "a
  shutdown you did not mean to start".
- **No admission control.** N holders means N VZ VMs and N guests' worth of
  committed RAM. A user who AirDrops four cartridges and boots them all can wedge
  the machine.
  *Realized.* Per-instance ports (W3) removed the accidental limit of one VM at a
  time, and nothing replaced it with a deliberate one. Documented as a
  limitation; unmitigated in code.
- **Two holders racing one cartridge.** Today the only mutual exclusion is a
  control-socket liveness probe, and `br disk pack` attaches at the same mountpoint
  `boot` uses. User-initiated mounts make this reachable.
  *Narrowed, not closed.* Three things helped: the control listener now takes an
  `O_EXCL` lock file before the dial-then-bind (removing the TOCTOU hole between
  two racing starts on one state dir); `br disk pack` and `br boot` no longer
  share a mountpoint (§6); and the mount watcher skips a volume an instance
  already holds. A fourth, later: `br disk pack` names the APFS volume after the
  cartridge's own file rather than after the source disk, so two cartridges
  packed from one base disk no longer claim one `/Volumes` path — which had made
  this reachable in ordinary use, not just through a user-initiated mount. Two
  *different* processes attaching the same image file remains possible.
- **GUI mode constrains holder threading** — the design depends on
  `DASessionSetDispatchQueue`, not `DASessionScheduleWithRunLoop`.
  *Retired.* `internal/diskarb` uses a serial dispatch queue and never touches a
  `CFRunLoop`, so the veto works in a headless holder and in one that has
  surrendered its main thread to `vz.StartGraphicApplication`.

