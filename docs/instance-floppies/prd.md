I have all the context I need. This is a self-contained PRD writing task using the provided design context and grounded map facts. Let me write the complete PRD.

# PRD: Instance Floppies

> **Status (2026-06-05):** Design doc, parked. Per latest direction we are keeping
> v1 straightforward: a multi-minute cold machine boot is acceptable for now, so the
> pre-baked `guest-image-latest` dependency (§8.F) is **deferred, not a prerequisite**.
> This PRD + the build scope + the review notes capture the validated design so the
> floppy build can resume later. Near-term effort is going to a simple menubar app
> over the existing CLI, not this feature.

**One-line summary:** A floppy is an Incus instance you carry as a DMG — insert it into the running bladerunner machine to run it, work, checkpoint as you go, and eject to a safe, atomically-stamped savefile.

---

## 1. Problem & motivation

bladerunner already gives you a long-lived Incus machine (`br start`) and, via #72, a way to ship a *whole VM* as an AirDrop-able cartridge. But the everyday unit a user actually cares about is not "the whole machine" — it's a single workload: a dev container, a demo environment, a scratch box. Today there is no portable, per-workload artifact:

- The #72 cartridge is the *heavy* case — a whole bootable VM (root.img + EFI + state + share), minutes to cold-boot, and overkill when you just want to hand someone one container.
- There is no way to snapshot one instance to a file, carry it, hand it to a colleague, or keep a pristine "golden" template of an instance and stamp out copies.
- There is no data-safety story for "I closed my laptop / the DMG got deleted / I overwrote it" at the instance granularity.

**Instance floppies** fill this gap: a *floppy* is one Incus instance stored on the host as a DMG, the everyday portable unit. You **insert** it (it imports into the machine's storage pool and runs from there), **work**, **checkpoint** (cheap, transparent, frequent), and **eject** (a small final flush to an atomically-written, identity-stamped savefile). The metaphor is deliberate and literal: the machine is the drive, the floppy is the disk, the files in the instance are the files on the floppy.

---

## 2. The mental model

| Concept | What it is |
|---|---|
| **The MACHINE** | The long-lived bladerunner Incus VM (`br start`). The drive. You rarely pass a whole machine — that's the rare heavy #72 cartridge. |
| **A FLOPPY** | An Incus **instance** (a system container in v1; a VM later), stored on the host as a **DMG**. The everyday portable unit. |
| **Files in the instance** | Files and directories *on the floppy*. |
| **`share/<name>/`** | A **separate** host↔guest data cable (VirtioFS) for live files — **not** where the instance lives. Optional / later phase. |

### The courier + fresco journey (multi-floppy)

The one machine runs several instances at once. Alice inserts `courier.dmg` (a web-service container) and `fresco.dmg` (a render box) into the same running machine. Each floppy:

- imports into the machine's btrfs pool and runs from there (not from its DMG),
- checkpoints independently back to its own DMG,
- optionally mounts its own `share/courier/` or `share/fresco/` data cable,
- gets its **own GUI window** via Incus (web-UI console / VNC), one per instance — *not* from VZ's single framebuffer (which is the machine's one console).

Alice ejects `fresco.dmg` mid-afternoon (final flush → detach) while `courier` keeps running. The machine is still up the whole time.

### Write-protect & templates (the three levers)

- **Pressed disk** (born read-only, UDZO/UDRO): immutable template. Insert it, work, then *eject "save-as"* a new writable DMG. The template stays pristine forever — the way you stamp out copies of a golden instance.
- **Write-protect tab** (a R/W DMG attached `-readonly`, per-insertion): run this session without persisting any changes back to the floppy.
- **Sealed cartridge case** (host file lock, `chflags uchg`): the DMG file itself can't be modified or deleted on the host.
- **Default** everyday floppy is a R/W DMG, updated in place at eject.

---

## 3. Goals

1. **A portable per-instance artifact**: one Incus instance ↔ one DMG, AirDrop-able, openable, runnable.
2. **Insert / work / checkpoint / eject** lifecycle, host-coordinated, that never crashes running work if the DMG is harmed.
3. **Cheap, transparent, frequent checkpoints** (the spike: ~4s/GB, non-pausing) so eject is a small final step and crash loss is bounded.
4. **Atomic, identity-stamped, verified write-back** — never silent data loss; worst case degrades to "saved to a side file + a loud warning."
5. **The three write-protect levers** mapped onto real DMG mechanics (read-only template, `-readonly` attach, host lock).
6. **Multi-floppy**: several instances at once, each from its own DMG, each with its own optional share and its own GUI.
7. **Reuse, not reinvent**: build on `internal/cartridge` (hdiutil), `internal/disk` (schema/catalog pattern), `internal/control` (RPC), `internal/incus` (REST/SDK), and the cloud-init provisioning — adding only the genuinely new pieces.

---

## 4. Non-goals

- **Running the instance rootfs directly on a VirtioFS-mounted DMG is OUT.** Spike #1 proved it: `incus storage create dir source=/mnt/share/pool` works, but `incus launch -s thatpool` fails permission-denied because VZ VirtioFS collapses all file ownership to a single host uid, while unprivileged Incus containers need real per-file idmap ownership (1000000+). Privileged containers are untested and undesirable. **The import/export model replaces this entirely** — the instance runs from the machine's pool; the DMG is a savefile.
- **Whole-VM #72 cartridges are not replaced.** They remain the separate, rare, heavy case (carry an entire machine). Floppies are the everyday lightweight case. The two coexist; see §9.
- **No new IPC transport, port, or Incus dependency.** Floppy flows ride the existing `127.0.0.1:18443` → vsock → guest `:8443` forwarder and the existing `lxc/incus/v6` SDK.
- **No conflation with `save`/`restore`** (those are whole-VM VZ machine-state snapshots, an orthogonal layer).
- **DiskArbitration Finder-eject coordination and the live `share/<name>/` data cable are not in v1** (see Open Decisions; deferred with recommendation).
- **Incus VMs (vs containers) are not in v1** (need nested virt + different export format).

---

## 5. Personas & top user journeys

**Persona A — the carrier.** Wants one workload on a DMG to run, edit, and keep.
**Persona B — the publisher.** Wants a pristine golden instance to stamp out copies of.
**Persona C — the careful operator.** Wants guarantees that nothing is ever silently lost.

### J1 — Insert & work
`br floppy insert courier.dmg` → bladerunner attaches the DMG, reads the small metadata file, streams the export tarball into the machine's btrfs pool via the SDK (`CreateInstanceFromBackup`), starts the instance. The instance runs from the pool. The DMG remains attached as a savefile target.

### J2 — Checkpoint (mostly invisible)
A periodic checkpoint goroutine snapshots + exports each attached floppy back to its DMG (`CreateInstanceSnapshot` + `CreateInstanceBackup`/stream → atomic write-back). Default cadence keeps the DMG never far behind. The user can also force one: `br floppy checkpoint courier`.

### J3 — Eject
`br floppy eject courier` → graceful `incus stop` of that one instance → final export → atomic, verified write-back into the still-mounted DMG → detach. The machine and other floppies stay up.

### J4 — Ship a read-only template
`br floppy new --template golden.dmg` (or `floppy seal`) produces a UDZO/UDRO pressed disk. Inserting it and ejecting forces **save-as** to a new writable DMG; the template is never mutated.

### J5 — Replace / delete-DMG safety (Persona C)
- If the DMG was **overwritten** since insert (identity stamp mismatch) → write to `<name>.conflict-<ts>.dmg` and warn; never silently overwrite.
- If the DMG was **deleted** → write to a recovery path and warn.
- Mid-session **corruption/deletion** of the DMG cannot crash the running instance (it runs in the pool, not on the DMG).

---

## 6. Functional requirements

### 6.1 Import/export lifecycle (the core model)

- **Storage on disk:** the DMG holds two things: (1) **floppy-owned sidecars** — the `Stamp` (UUID + generation) and the floppy `Manifest` (instance name, image base, idmap summary, `incus_version`, `backup_format` = `portable|optimized`, payload digest); and (2) the **payload** = the *unmodified* native `incus export` backup tarball. bladerunner never reconstructs the tarball from parts — the "metadata Incus needs to run it" lives **inside** the backup tarball's own `index.yaml`; the floppy sidecars are bladerunner's, separate from Incus's. (Correction from review: there is no bladerunner-side reassembly of the tarball.)
- **Insert** = attach DMG → **preflight** (`HasExtension` for `container_backup_override_pool` + `backup_override_name`; compare the Manifest's `incus_version`/`backup_format` against the machine — fail fast with "this floppy needs a newer machine" rather than an opaque import error) → `CreateInstanceFromBackup(InstanceBackupArgs{BackupFile: payload, PoolName: <machine pool>, Name})` → start. The instance runs from the machine's pool. **Refuse** an `optimized` payload when the target pool driver is `dir` (btrfs-send backups are not restorable into a dir pool) — clear error, no silent failure.
- **Checkpoint** = `CreateInstanceSnapshot(checkpoint-<ts>)` + **`CreateInstanceBackup` (materialize) then `GetInstanceBackupFile` (download)** → atomic write-back into the mounted DMG. **Important (review correction): `incus export` is materialize-then-download, not a stream** — `CreateInstanceBackup` writes a *full* backup tarball onto the pool's backup storage first, so a checkpoint of an N-GB floppy transiently needs **~N GB free on the pool** on top of the live instance. Always `DeleteInstanceBackup` the server-side record in a `defer` so failed/partial checkpoints don't leak pool space. `OptimizedStorage` (btrfs send) is set **only when the pool driver is actually btrfs** (queried at runtime), and is used for **fast intra-machine checkpoints**; the **default eject savefile is `portable` (non-optimized)** so a floppy imports anywhere.
- **Eject** = `UpdateInstanceState(Stop)` on the one instance → final **portable** export → atomic verified write-back → `hdiutil` detach. Whether the pool instance is **deleted** on eject is explicit (recommend: delete on a clean eject, so re-inserting the same DMG doesn't collide on instance name; keep it on `--keep` for fast re-insert).
- **Pool free-space precondition** (review): before any checkpoint/eject, assert `pool free ≥ payload size + headroom`; on `ENOSPC` during backup-create, abort, keep the last good DMG (`.prev`), and warn loudly — a first-class anti-clobber case. The machine's btrfs pool size is provisioned as a function of expected floppy count (`Σ live floppies + max single backup + headroom`), **not** a fixed 20 GiB.
- **Drive via the SDK, not SSH.** All insert/checkpoint/eject use `internal/incus.Client` over the already-wired loopback→vsock REST path (the choice the repo already made for `ls/exec/events/logs`). New thin wrapper methods (`ImportInstance`, `ExportInstance`, `SnapshotInstance`, `SetInstanceState`, `DeleteInstance`) wrap the already-imported `incusclient.InstanceServer` primitives. **All long operations use `op.WaitContext(ctx)`** so checkpoint/eject are cancellable and time-bounded (backup create/import are ~4s/GB).

### 6.2 The btrfs machine pool (fast, non-pausing export)

- The machine's default Incus storage pool must be **btrfs** (loopback file, no extra disk), not the current `DRIVER=dir`. This is what makes export ~4s/GB and non-pausing (spikes #2, #3) and enables native incremental `send -p` for very large floppies.
- Implementation: add `btrfs-progs` to the apt install block (`cloudinit.go:346–356`, both native-trixie and zabbly branches) and replace bare `incus admin init --auto` (`cloudinit.go:372`) with an explicit btrfs pool create / preseed.
- **No silent `|| true`:** if the btrfs pool create fails (missing `btrfs-progs`, kernel lacks btrfs), fall back to `dir` *with a loud log*, never an Incus left with no usable default pool.
- The same canonical btrfs-init form must be applied identically in **all** provisioning paths to avoid silent drift: (a) cloud-init bootstrap, (b) `build-guest-image.sh` virt-customize path, (c) its chroot fallback, (d) the future `br-agent` init.

### 6.3 Checkpoint cadence

- A periodic checkpoint goroutine (a `time.Ticker` started in `runStart` after the VM is up and the Incus client connects, cancellable via the `runStart` ctx) checkpoints each attached floppy.
- **Per-floppy concurrency lock**: a checkpoint must not overlap a user eject or another checkpoint of the same instance (no such lock exists today — build it).
- Default cadence and on-by-default: see Open Decision §8.B (recommend **on by default, every 5 minutes**, configurable, with `--no-checkpoint` to disable per insert).

### 6.4 Atomic + identity-stamped write-back (data safety)

This entire surface is greenfield; the existing `assets.go` rename helper is rename-only (no fsync/`.prev`/identity). Build:

- **Atomic verified write-back:** export to a temp file inside the DMG → **fsync** → verify (size/sha256) → **atomic rename** over the canonical name → keep exactly **one `.prev`**. **Never delete the VM-pool copy until the export is confirmed.**
- **Identity stamp:** each DMG carries a **UUID + generation counter** (a JSON stamp file inside the DMG; generate the UUID via `crypto/rand`+hex — no `google/uuid` module needed, matching repo convention). On eject, compare against the stamp captured at insert:
  - changed since insert → `<name>.conflict-<ts>.dmg` + warn, never overwrite;
  - DMG deleted → recovery path + warn.
- **Bounded crash loss:** a crash with no eject loses only changes since the last checkpoint (bounded by cadence). The VM pool persists across normal machine reboots.

### 6.5 The three write-protect levers

- **Born read-only (pressed disk):** `ConvertToDMG` (UDZO) already exists; eject of a read-only-source must enforce **save-as a new writable DMG** (a new "never write back to a read-only source" guard — new logic).
- **Write-protect tab (`-readonly` attach):** **new code** — `attachArgs` (`cartridge.go:165`) hardcodes a writable mount; add an `AttachReadOnly` variant / threaded flag without breaking existing single-arg `Attach` callers.
- **Sealed case (`chflags uchg`):** a host-level file lock on the DMG.
- **Default R/W DMG:** updated in place at eject.

### 6.6 Multi-floppy

- Multiple instances run at once, each ↔ its own DMG, each with optional `share/<name>/`.
- **New per-floppy registry** keyed by **instance name AND DMG identity (UUID/generation)** — nothing maps instance↔DMG today. Mirror the `internal/disk` manifest/catalog pattern (a `.floppy` manifest + catalog), reusing `disk.ValidName`/`disk.ValidSHA256` validators.
- Floppies are **not** control-socket slots. `resolveEjectSlot`'s VM-slot scan does **not** apply; floppy listing comes from the Incus instance list + the registry. Use a **separate mnt subtree (e.g. `mnt-floppy/`) or a typed marker file** so the existing whole-VM cartridge scanners (`listAttachedCartridges`, `resolveEjectSlot`) don't mis-detect a floppy as a bootable cartridge.
- **Per-instance GUIs** come from Incus (web-UI console / VNC), one window each — not VZ's single framebuffer.

### 6.7 Repo-convention requirements (scope guardrails)

- New darwin-only host bits (e.g. the `-readonly`/`chflags` paths, any DiskArbitration work) behind `//go:build darwin` with `!darwin` stubs returning `ErrUnsupported`, so **GOOS=linux CI stays clean**. Follow the `cartridge_darwin.go`/`cartridge_other.go` split verbatim.
- No new heavy deps. `golangci-lint(latest)` clean: keep new exported API referenced on Linux (the unused-code trap the stubs exist to avoid); watch `gocyclo<=25` on the new orchestration funcs and `unparam` on helpers.
- Prove host behavior live with a `make smoke-floppy` script mirroring `scripts/smoke-cartridge.sh`: btrfs pool comes up → instance launches on it → checkpoint exports to DMG → eject write-back is atomic+stamped → re-insert round-trips.

---

## 7. The evidence base (validation)

This design is not speculative — four live spikes anchor it:

1. **Live-rootfs-on-VirtioFS is dead** → justifies the import/export model. `incus launch` on a VirtioFS-backed `dir` pool fails permission-denied: VZ VirtioFS collapses all ownership to one host uid; unprivileged Incus needs real per-file idmap (1000000+).
2. **btrfs pool inside the VM works perfectly** → justifies the btrfs machine pool. `apt install btrfs-progs` (v6.14) + `incus storage create pool btrfs size=20GiB` makes a loopback-file pool (no extra disk); a uid-shifted Debian container launches fine.
3. **Export is cheap and non-pausing** → justifies frequent, transparent checkpoints. Export of a running ~950MB btrfs instance: ~4.1s full and ~4.1s `--optimized-storage` (btrfs send), ~230 MB/s, **did not pause** the container (0.2s in-container heartbeat showed max gap 0.203s). ~4s/GB, no rsync. `send -p` gives native incremental for large floppies.
4. **Cold-init is too slow for the floppy UX** → justifies the pre-baked guest image dependency. The cartridge's Incus took ~7 minutes to cold-init on first boot. "Open DMG → pick a floppy → go" needs a **pre-baked guest image** (Incus already installed+initialized, btrfs-progs + btrfs default pool baked in).

---

## 8. Open decisions (with recommendations)

**A. Verb naming.**
**Recommend a new `br floppy` noun** with sub-verbs: `floppy insert | eject | list | new | checkpoint | save-as`. Rationale: overloading `disk`/`boot`/`eject` collides with #71 (`.disk` manifests / `boot` a VM) and #72 (`boot <cartridge>` / whole-VM `eject`), and reusing `save` collides with VZ machine-state `save`. A distinct noun keeps the three artifact families (disk / cartridge / floppy) cleanly separated.

**B. Checkpoint default cadence + on-by-default.**
**Recommend on-by-default, every 5 minutes**, configurable via flag/config, with `--no-checkpoint` to opt out per insert. Rationale: spike #3 shows checkpoints are ~4s/GB and non-pausing, so the cost is negligible and the safety win (bounded crash loss) is large. Honest tradeoff: very large floppies on slower storage could see checkpoint overlap — the per-floppy lock + `send -p` incremental mitigate this; expose the cadence so power users can lengthen it.

**C. Live `share/<name>/` data cable — v1 or later?**
**Recommend defer past v1.** The VirtioFS share is already wired at the machine level and the `disk.ShareSpec` shape can be reused later, but it's orthogonal to the core insert/checkpoint/eject value and adds per-floppy mount lifecycle. Ship the savefile model first; add the live cable as a fast-follow.

**D. DiskArbitration Finder-eject coordination — v1 or later?**
**Recommend defer past v1.** It requires a substantial new cgo surface (DiskArbitration + CoreFoundation frameworks, a `CFRunLoop` on a dedicated thread for the session) — a notable departure from the repo's pure-Go + hdiutil-exec approach. Critically, the import/export model already makes the DMG a savefile, so a Finder force-eject mid-session only loses changes since the last checkpoint; it does **not** crash the running instance. Honest tradeoff: until this lands, a user who force-ejects in Finder gets a stale-by-one-checkpoint DMG and a detached image — acceptable, and clearly documented, for v1.

**E. Container-only in v1, or Incus VMs too?**
**Recommend container-only in v1.** Incus VM export differs (needs nested virt — gated on `ConfigKeyNestedVirt` — and a different rootfs format). Container-only avoids that surface. Design the export/import wrappers to **not hardcode container assumptions**, so VM floppies are an additive v2.

**F. Pre-baked `guest-image-latest` + btrfs — hard prerequisite or parallel track?**
**Recommend parallel track, hard prerequisite for the *good* UX, not for correctness.** The host DMG surface (insert/checkpoint/eject/anti-clobber) is independent and can be built and tested against a cold-init machine. But the "open DMG → go" experience and the cheap-checkpoint numbers depend on (a) the btrfs pool and (b) booting from a pre-baked image instead of the ~7-min cold-init. The publish machinery already exists (`build-guest-image.yml` maintains `guest-image-latest`); the gaps are: it hasn't been run, the artifact may carry a `-no-agent` suffix that won't match `HostedGuestImageURL`'s expected `bladerunner-guest-<arch>.qcow2` name, and `useHosted` defaults `false` (`config.go:280`). **Do not flip `useHosted=true` until the artifact name/agent situation is reconciled, or the download 404s.** Track this as a coupled dependency, ship floppy code in parallel, and gate the "fast UX" milestone on the published btrfs image.

**G. Coexistence with #72 whole-VM cartridges.**
**Recommend explicit separation, shared low-level reuse.** Floppies reuse `internal/cartridge`'s hdiutil mechanics but live in their own mnt subtree / typed marker and their own `.floppy` registry so the whole-VM cartridge scanners never mistake a floppy for a bootable cartridge. Messaging: cartridge = carry a *machine* (rare, heavy); floppy = carry an *instance* (everyday, light).

---

## 9. Dependencies & risks

- **Pre-baked `guest-image-latest` + btrfs default pool** (spike #4): hard dependency of the good UX. btrfs-init must be applied in up to **four divergent places** (cloud-init bootstrap, virt-customize path, chroot fallback, future `br-agent`) — miss one and that path silently gets a `dir` pool. **Bake-pool-at-build-time vs init-on-first-boot is an unresolved fork**: baking a loopback btrfs pool inflates the qcow2 and may not survive `virt-sparsify`; first-boot init keeps the image small but reintroduces some of the cold-init cost the pre-bake is meant to kill. **Recommend: bake `btrfs-progs` into the image, but init the loopback pool on first boot via systemd-firstboot/`br-agent`** — small image, single quick init, fallback-to-dir-with-loud-log on failure.
- **`-no-agent` artifact naming mismatch**: `HostedGuestImageURL` expects the bare `bladerunner-guest-<arch>.qcow2`; the published artifact may carry `-no-agent`. Reconcile before flipping `useHosted`.
- **idmap/btrfs assumptions**: the whole model rests on unprivileged Incus needing real idmap ownership (spike #1) and btrfs delivering non-pausing optimized export (spike #3). If a target kernel lacks btrfs, the loud-fallback-to-dir path keeps correctness (at the cost of slower, pausing exports).
- **DiskArbitration cgo surface**: deferred (§8.D); flagged as substantial if/when pursued.
- **Data-integrity sensitivity**: fsync ordering, verify-before-rename, one-`.prev`, and the "never delete the VM-pool copy until export confirmed" invariant are entirely new and must be gotten exactly right — this is the load-bearing safety surface.
- **Timing window on eject**: the final export must complete while the DMG is still attached and the guest still live (before `cancel()` + the LIFO detach defer). The `saveCommandTimeout=10min` covers long handlers, but the client-side `waitForSocketGone` margin (`ejectWaitMargin=15s`, sized for VMM teardown) must be raised to cover a real multi-GB export.
- **Slot-resolution category error**: a floppy is an instance inside the one machine, not a control-socket slot; reusing `resolveEjectSlot` for floppies would mis-detect. New registry + listing required.

---

## 10. Success metrics

1. **Round-trip integrity:** insert → work → checkpoint → eject → re-insert reproduces the instance bit-for-identical (config + rootfs) in 100% of `make smoke-floppy` runs.
2. **Anti-clobber correctness:** in fault-injection tests (DMG overwritten / deleted / read-only / force-detached mid-session), **zero silent data loss** — every case degrades to a clearly-warned side file or recovery path, and the running instance never crashes.
3. **Checkpoint cost:** median checkpoint ≤ ~4s/GB and **non-pausing** (in-instance heartbeat max gap < 0.5s), matching spike #3, on the btrfs machine pool.
4. **Bounded crash loss:** after an unclean shutdown, recovered state is never older than one checkpoint interval.
5. **Fast insert UX (gated on §8.F):** with the pre-baked btrfs image, "machine up → `floppy insert` → instance running" completes in seconds, not the ~7-min cold-init.
6. **Multi-floppy:** two instances (`courier` + `fresco`) from two DMGs run, checkpoint, and eject **independently** while the machine stays up, each with its own GUI window.
7. **CI/convention health:** darwin + `GOOS=linux` both build clean, `golangci-lint(latest)` + Trivy pass, and host behavior is proven by `make smoke-floppy` (mirroring `smoke-cartridge`).