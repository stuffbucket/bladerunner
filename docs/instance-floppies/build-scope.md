
# BUILD SCOPE — Instance Floppies

Phased, PR-by-PR implementation plan for the Instance Floppies PRD, grounded in the bladerunner codebase as it stands on `main`. Each phase is independently shippable. Real file paths and line anchors are cited so an implementer can start immediately.

---

## 0. Architecture overview

### Verb naming (resolves Open Decision §8.A)

A new top-level **`br floppy`** noun with sub-verbs. This keeps the three artifact families cleanly separated and avoids the real collisions the PRD flags:

| Family | Verbs (existing/new) | Artifact |
|---|---|---|
| **disk** (#71) | `disks`, `boot`, `disk new`, `disk bake` | `.disk` JSON manifest + image |
| **cartridge** (#72) | `disk pack --ship`, `boot <cartridge>`, `eject` | whole-VM `.sparseimage`/`.dmg` |
| **floppy** (NEW) | `floppy insert`/`eject`/`list`/`new`/`checkpoint`/`save-as`/`seal` | one Incus instance as a `.dmg` |

`br eject` (whole-VM, control-socket slot) is **left untouched**; floppy eject is `br floppy eject <name>` and operates on an Incus instance inside the running machine, never on a control-socket slot. The word `save` stays reserved for VZ machine-state (`save.go`/`restore.go`); floppy persistence is `checkpoint`/`eject`.

### New packages & types

- **`internal/floppy`** (NEW package) — the floppy DMG format, identity stamp, atomic write-back, and registry. Mirrors the `internal/cartridge` platform-split skeleton: `floppy.go` (platform-neutral logic + types) + `floppy_darwin.go` / `floppy_other.go` (host gating). Even though `internal/cartridge` already provides the hdiutil mechanics, the *floppy-specific* logic (stamp, atomic verified write-back, manifest schema, registry) is greenfield and belongs in its own package.
  - `Manifest` — the small read-mostly metadata Incus needs (instance name, image base, profile, devices, idmap, instance UUID, export digest, payload filename). Schema/validate/parse/load/clone **mirrors** `internal/disk/manifest.go` (do not import-reuse — different shape). Reuses `disk.ValidName` / `disk.ValidSHA256` validators directly.
  - `Stamp` — `{UUID string; Generation uint64}` JSON sidecar inside the DMG. UUID via `crypto/rand`+hex (repo convention — **no `google/uuid`**; closest prior art `internal/vm/metadata.go`).
  - `Registry` / `RegistryEntry` — host-side map of `instance name + Stamp(UUID,generation)` ↔ mounted DMG path ↔ pool. Persisted under the state dir. **Nothing maps instance↔DMG today**; this is net-new. Catalog/overlay pattern **mirrors** `internal/disk/catalog.go`.
  - `WriteBack(ctx, dmgMount, payload, expectStamp)` — the load-bearing data-safety primitive: temp-in-DMG → **fsync** → verify (size+sha256) → atomic rename over canonical → keep exactly one `.prev`; conflict/recovery side-file on stamp mismatch/deletion. Starting points: `internal/vm/assets.go:438` (temp+rename, **no fsync/.prev** — extend) and `internal/vm/metadata.go` (JSON sidecar).
  - Floppy sizing constants (`HeadroomGiB`/`MinSizeGiB` in `cartridge.go:45` are VM-scale; floppies want their own small values — sparse cost is real-bytes-only, so over-provisioning is cheap, but the floor should be export-tarball-appropriate).

- **`internal/incus`** (EXTEND, not new) — add thin wrapper methods on the existing `Client` (`internal/incus/instance.go`), each following the established `ExecInstance` shape (build `api.*Post`, `op := server.X(...)`, `op.WaitContext(ctx)`):
  - `ImportInstance(ctx, backup io.Reader, pool, name)` → `CreateInstanceFromBackup(InstanceBackupArgs{...})`
  - `ExportInstance(ctx, name, w io.WriteSeeker, optimized bool)` → `CreateInstanceBackup` + `GetInstanceBackupFile` (or `CreateInstanceBackupStream`), `OptimizedStorage` for btrfs send
  - `SnapshotInstance(ctx, name, snap)` → `CreateInstanceSnapshot`
  - `SetInstanceState(ctx, name, action)` → `UpdateInstanceState` (stop/start/freeze)
  - `DeleteInstance(ctx, name)` → `DeleteInstance`
  - These wrap the already-imported `lxc/incus/v6@v6.23.0` `InstanceServer`. **No new transport/port/dependency.** Wrappers must **not** hardcode container assumptions (keeps VM floppies a v2 additive — Open Decision §8.E).

- **`cmd/bladerunner/floppy.go`** (NEW) — the `floppy` cobra command tree and orchestration (insert/list/new/checkpoint/save-as/seal CLI-side). Reuses `connectIncus()`/`incusClientFromControl()` (`incus_client.go`) verbatim, and `emitJSON`/`jsonOrError`/`instanceReport`-style output helpers from `output.go`/`ls.go`.

### What is reused from each existing package

| Package | Reused as-is | Mirrored (copy pattern) | Extended (new code) |
|---|---|---|---|
| `internal/cartridge` | `Create`, `Attach`, `Detach` (busy→-force), `Compact`, `ConvertToDMG` (UDZO template), `ConvertToSparse` (writable copy), `IsAttached`, `Mount`, the darwin/`!darwin` `hostSupported()` split | — | **`AttachReadOnly`** / `-readonly` flag through `attachArgs` (`cartridge.go:165` hardcodes writable — new); a floppy-sized `SizeGiB` |
| `internal/disk` | `ValidName`, `ValidSHA256`, `nameRe`, `sha256Re`, `ShareSpec` shape | `Manifest`/`Validate`/`Parse`/`Load`/`Clone` + `Catalog` (builtins+XDG overlay) | — |
| `internal/control` | `Router.Mount` sub-router, `registerUpgradeHandlers` wiring pattern, `getRunner`/`setRunner` mutex holder, `client.Send`, `saveCommandTimeout` | — | `floppy.*` sub-router commands; raise client-side `ejectWaitMargin` for floppy eject |
| `internal/incus` | `Connect`/`ConnectFromFiles`, `Server()` escape hatch, the loopback→vsock forwarder | — | the 5 wrapper methods above |
| `internal/provision` | native-vs-zabbly apt branching, `incus admin waitready` loop | — | `btrfs-progs` + canonical btrfs pool init (Phase 0) |
| `internal/vm` | `assets.go` temp+rename idiom, `metadata.go` crypto/rand JSON sidecar | — | fsync + `.prev` + stamp + conflict side-file (in `internal/floppy`) |

---

## Phase 0 — btrfs machine pool provisioning [+ pre-baked guest image track]

**Why first:** spikes #2/#3 (non-pausing ~4s/GB export) and the cheap-checkpoint UX *depend on* a btrfs default pool. This phase is independent of all host DMG code and unblocks the performance numbers in Phases 1–2. It is the one phase with cross-track sequencing risk (the pre-baked image), so it starts early and runs partly in parallel.

### Decision: bake `btrfs-progs`, init pool on first boot (resolves §9 fork + §8.F)

Bake the **package** into the image but **init the loopback pool on first boot** (via the canonical init step), not at image-build time. Rationale: a build-time loopback pool inflates the qcow2 and may not survive `virt-sparsify` (`build-guest-image.sh:192`); a single quick first-boot `incus storage create pool btrfs size=NNGiB` keeps the image small for one bounded init. Fallback-to-`dir`-with-loud-log on failure, never a silent `|| true` that leaves Incus with no usable pool.

### The single canonical btrfs-init form (applied identically in all 4 paths)

Define one shell snippet: `incus admin waitready` (reuse existing loop) → if no usable default pool, `incus storage create <pool> btrfs size=NNGiB`; **on failure, fall back to `incus storage create <pool> dir` and emit a loud `WARNING: btrfs pool create failed, falling back to dir (slow, pausing exports)` log — not `|| true`.** Replace the bare `incus admin init --auto || true`.

### Files (edited)

- `internal/provision/cloudinit.go:348,349-352,355` — append `btrfs-progs` to all three apt/dnf install invocations. Pure literal text in the `fmt.Sprintf` template — **mind `%%` escaping** (`cloudinit.go:430` precedent); no new format args.
- `internal/provision/cloudinit.go:372` — replace `incus admin init --auto || true` with the canonical btrfs-init snippet. Keep the existing `waitready` loop (365–370) ahead of it.
- `internal/provision/cloudinit.go:538-557` (`buildMinimalCloudInit`, the `UseGuestAgent=true` path) — this path defers to `br-agent`, so the canonical init must **also** land in the agent's `admin init` logic (documented at `internal/agent/protocol.go:33`; `cmd/br-agent` does not yet exist — leave a tracked TODO + the shared snippet ready so the agent path doesn't silently get a `dir` pool).
- `scripts/build-guest-image.sh:125` — add `btrfs-progs` to the virt-customize `--install` list; **do not** bake the pool (decision above). `:175` — same `btrfs-progs` addition in the nbd+chroot fallback to avoid divergence.
- `scripts/build-guest-image.sh` — leave the first-boot pool init to the agent/cloud-init canonical snippet (image stays small; no `TARGET_SIZE_GIB:33` bump needed).

### Pre-baked image track (parallel, gated)

- The publish machinery already exists (`.github/workflows/build-guest-image.yml:108-161` maintains `guest-image-latest`). **Do not flip `useHosted=true` (`internal/config/config.go:280`) yet.** Two blockers, both tracked here, neither blocking floppy code: (a) `guest-image-latest` is unpublished; (b) the artifact may carry a `-no-agent` suffix (`build-guest-image.yml:75-78,84`) that won't match the bare `bladerunner-guest-<arch>.qcow2` name `HostedGuestImageURL` (`config.go:253-262`) expects → 404. Reconcile artifact naming before flipping. Per-disk opt-in (`internal/disk/apply.go:27-31`) remains available for testing without touching the global default.

  **Update (#155):** the default IS now flipped to the pre-baked image + agent handshake, held for the #154 real-hardware boot-verify. Both blockers above are now handled at runtime rather than gating the flip: a 404 (missing/renamed asset) or any download/verify failure emits a `WARN` and auto-falls-back to the pinned Debian + cloud-init path (`internal/vm/assets.go` `ensureHostedOrFallback`), and the pre-baked checksum is verified fail-closed against its `.sha256` sidecar. The forced-cloud-init escape hatch (`--cloud-init` / `BLADERUNNER_FORCE_CLOUD_INIT`) restores the old default explicitly.

### Verbs/flags

None (provisioning only).

### Test strategy

- **New `scripts/smoke-floppy.sh` (stub) + `make smoke-floppy`** — Phase 0 slice asserts only: machine boots → `incus storage list` shows a **btrfs** default pool → a uid-shifted container launches on it (spike #2). Mirrors `scripts/smoke-cartridge.sh` / `Makefile:97-98`.
- Unit: a render test asserting the bootstrap script contains `btrfs-progs` and the canonical init in every branch (catches the brittle `fmt.Sprintf` template — a class only caught by running the bootstrap, so assert the rendered string).

### Build-tag / GOOS=linux obligations

`cloudinit.go` is pure string-building, **no build tags**, already builds on linux — safe. No host-side code added in this phase, so no darwin gating needed. CI stays clean.

### Effort / risk

**Effort: M. Risk: M.** Risk concentrated in the 4-path drift (miss one → silent `dir` pool) and the `|| true` swallow-the-error trap. The loud-fallback log + the smoke assertion mitigate.

---

## Phase 1 — instance-DMG format + insert/import + state tracking

**Why next:** establishes the artifact, the import path, and the registry that every later phase keys off. Independent of Phase 2's loop; ships as "insert a floppy, it runs" with checkpoint/eject deferred.

### Files

- **`internal/floppy/floppy.go`** (new) — `Manifest` (mirror `disk/manifest.go:44,112,197,209,170`), `Stamp`, sizing constants, format constants. Reuse `disk.ValidName`/`disk.ValidSHA256`.
- **`internal/floppy/floppy_darwin.go`** / **`internal/floppy/floppy_other.go`** (new) — `hostSupported()` true/false + `ErrUnsupported` sentinel; every public fn early-returns it on `!darwin`. **Copy `cartridge_darwin.go`/`cartridge_other.go` verbatim.**
- **`internal/floppy/registry.go`** (new) — `Registry` keyed by `instance name + Stamp`. Persisted JSON under `config.DefaultStateDir()`. Uses a **separate `mnt-floppy/` subtree** (or a typed marker file) so `listAttachedCartridges`/`resolveEjectSlot` (`cartridge.go:480`, `eject.go:96`) never mis-detect a floppy as a bootable cartridge (PRD §6.6, §8.G).
- **`internal/incus/instance.go`** (edit) — add `ImportInstance` (+ stub `ExportInstance`/`SnapshotInstance`/`SetInstanceState`/`DeleteInstance` so the Phase-2 surface compiles referenced; keep exported API used on Linux to dodge the unused-code trap).
- **`internal/cartridge/cartridge.go`** (edit) — add floppy-sized `SizeGiB` variant or a `SizeGiBFor(bytes)` helper (don't reuse the VM-scale `HeadroomGiB=8`/`MinSizeGiB=10`).
- **`cmd/bladerunner/floppy.go`** (new) — `floppy` command tree; `floppy insert <name.dmg>`, `floppy new <name>`, `floppy list`.

### Insert flow (J1)

`floppy insert courier.dmg` → `cartridge.Attach` the DMG (into `mnt-floppy/<name>`) → read `Manifest` + `Stamp` → `connectIncus()` → `incus.ImportInstance(ctx, payloadReader, machinePool, name)` (`CreateInstanceFromBackup`, `op.WaitContext(ctx)`) → `SetInstanceState(Start)` → record `RegistryEntry{name, stamp, dmgPath, pool}`. Instance runs from the machine's btrfs pool; the DMG stays attached as the savefile target. **Stream over the existing `127.0.0.1:18443`→vsock forwarder** — no SSH `incus import`.

### Verbs/flags

- `floppy insert <path> [--no-checkpoint] [--readonly]` (`--readonly` wired but enforced in Phase 3; `--no-checkpoint` wired for Phase 2).
- `floppy new <name> [--size N]` — create a blank R/W floppy DMG with a fresh `Stamp`.
- `floppy list` — joins `incus.ListInstances` + the registry (NOT `resolveEjectSlot`); JSON via `emitJSON` (`output.go`).

### Test strategy

- Unit (platform-neutral, runs on Linux): `Manifest` validate/parse/clone; `Stamp` generation + JSON round-trip; registry add/lookup/remove keyed by name+stamp; `SizeGiBFor`.
- `make smoke-floppy` (extends Phase 0): build a tiny floppy DMG → `floppy insert` → assert the instance appears in `incus list` running on the btrfs pool → `floppy list` shows it mapped to the DMG.

### Build-tag / GOOS=linux obligations

All hdiutil/attach paths behind `floppy_darwin.go`; `floppy_other.go` returns `ErrUnsupported`. `Manifest`/`Stamp`/`Registry` are pure Go (no tags) and **must stay referenced on Linux** (call them from the `!darwin` stub paths or tests) to satisfy `golangci-lint(latest)` unused checks. Watch `gocyclo<=25` on `runFloppyInsert`.

### Effort / risk

**Effort: L. Risk: M.** The registry design (name+stamp keying, mnt-floppy isolation) is the load-bearing new abstraction; getting the namespace separation right prevents cartridge scanners mis-detecting floppies.

---

## Phase 2 — checkpoint loop + eject (the core lifecycle + data safety)

**Why:** turns "insert and run" into the full insert/work/checkpoint/eject value, with the anti-clobber guarantees that are the PRD's headline. Depends on Phase 1 (registry, import wrapper) and benefits from Phase 0 (btrfs → non-pausing export).

### Files

- **`internal/incus/instance.go`** (edit) — flesh out `ExportInstance` (`CreateInstanceBackup`+`GetInstanceBackupFile`/`CreateInstanceBackupStream`, `OptimizedStorage=true`, `op.WaitContext(ctx)`), `SnapshotInstance`, `SetInstanceState(Stop)`.
- **`internal/floppy/writeback.go`** (new) — `WriteBack`: export→temp-in-DMG→**fsync**→verify(size+sha256)→atomic rename→one `.prev`. Identity-stamp compare; on mismatch write `<name>.conflict-<ts>.dmg`, on DMG-deleted write a recovery path, both with loud warnings. **Invariant: never delete the VM-pool copy until export is confirmed.** Per-floppy `sync.Mutex` so a checkpoint can't overlap an eject or another checkpoint of the same instance (no such lock exists today).
- **`internal/control/control.go`** (edit) — add `CmdFloppyCheckpoint`/`CmdFloppyEject` (or a mounted `floppy.*` sub-router via `Router.Mount`, mirroring `config_handler.go:79-81`).
- **`cmd/bladerunner/start.go`** (edit) — in `registerUpgradeHandlers` (`:88`), register the floppy checkpoint/eject handlers (reach the runner via `getRunner()` + `cfg`, same as `CmdSave`/`CmdEject`). In `runStart` (`:144`), after the VM is up and the Incus client connects (same place `ProbeGuest` attaches), start the **checkpoint goroutine** (`time.Ticker`, cancellable via the `runStart` ctx at `:145`).
- **`cmd/bladerunner/floppy.go`** (edit) — `floppy checkpoint <name>`, `floppy eject <name>`.
- **`cmd/bladerunner/eject.go`** (edit) — raise the client-side wait margin for floppy eject: the existing `ejectWaitMargin=15s` (`:89`) is sized for VMM teardown only; a multi-GB final export needs a larger, export-sized budget. Server-side `saveCommandTimeout=10min` already covers the long handler.

### Checkpoint flow (J2) — on by default, every 5 min (resolves §8.B)

Goroutine ticks every 5 min (configurable; `--no-checkpoint` per-insert opt-out). Per attached floppy, under its mutex: `SnapshotInstance` → `ExportInstance(optimized=true)` → `floppy.WriteBack`. Spike #3 (~4s/GB, non-pausing, in-container max gap 0.203s) makes this cheap; `send -p` incremental mitigates very large floppies. `floppy checkpoint <name>` forces one.

### Eject flow (J3) — ordering is load-bearing and already correct

`floppy eject courier` → control `CmdFloppyEject` handler runs **while the guest is still live and the DMG still mounted** (the machine's VMM is not torn down — floppy eject does NOT `cancel()` runStart or `runner.Stop()`; it operates on one Incus instance, leaving the MACHINE up): acquire per-floppy lock → `SetInstanceState(Stop)` (graceful) → final `ExportInstance` → `floppy.WriteBack` (atomic+stamped) → `cartridge.Detach` (busy→-force) → remove registry entry. Other floppies and the machine stay running (`fresco` keeps going while `courier` ejects).

### Anti-clobber (J5, Persona C)

Stamp captured at insert vs at eject: changed → conflict side-file + warn (never overwrite); deleted → recovery path + warn; mid-session DMG corruption/deletion cannot crash the instance (it runs in the pool). Bounded crash loss = one checkpoint interval.

### Test strategy

- Unit (Linux-clean): `WriteBack` against a temp dir simulating a mounted DMG — verify-before-rename, `.prev` retention (exactly one), fsync ordering, conflict-side-file on stamp mismatch, recovery path on deleted target, never-delete-source-until-verified. **Fault injection** for §10.2 (overwritten/deleted/read-only/force-detached) → assert zero silent loss. Per-floppy lock contention test.
- `make smoke-floppy` (extends): insert → write a file in the instance → `floppy checkpoint` → assert DMG payload updated atomically + stamp generation incremented → `floppy eject` → re-insert → assert config+rootfs bit-identical (§10.1 round-trip). Multi-floppy: `courier`+`fresco` insert, independent checkpoint/eject, machine stays up (§10.6).

### Build-tag / GOOS=linux obligations

`WriteBack` is pure Go (fsync/rename/sha256) — **no build tags, runs in Linux CI** (this is where the data-safety unit tests live). Attach/detach stays darwin-gated. Watch `gocyclo<=25` on the eject handler and the checkpoint loop; `unparam` on `WriteBack` helpers.

### Effort / risk

**Effort: XL. Risk: H.** This is the load-bearing data-integrity surface — fsync ordering, verify-before-rename, one-`.prev`, never-delete-pool-copy-until-confirmed, and the eject timing window (export must finish before detach) must be exactly right. Highest-scrutiny phase; the fault-injection suite is the gate.

---

## Phase 3 — write-protect / templates / save-as

**Why:** the three levers (Persona B golden templates). Depends on Phase 2's eject path (where save-as branches). Independently shippable.

### Files

- **`internal/cartridge/cartridge.go`** (edit) — add `AttachReadOnly` (or thread a `-readonly` flag through `attachArgs:165`) **without breaking existing single-arg `Attach` callers**. The write-protect tab.
- **`internal/floppy/floppy.go`** (edit) — `Seal`/`Unseal` (`chflags uchg` host lock, darwin-gated) — the sealed case; a "born read-only template" guard that **enforces save-as** (never write back to a read-only source — new logic; `ConvertToDMG`/UDZO already produce the pressed disk, `ConvertToSparse` produces the writable copy, but the "never mutate a read-only source" guard is new).
- **`cmd/bladerunner/floppy.go`** (edit) — `floppy new --template`, `floppy seal`, `floppy save-as`; eject-of-readonly auto-routes to save-as.

### Verbs/flags

- `floppy new --template <name.dmg>` / `floppy seal <name.dmg>` — UDZO/UDRO pressed disk (J4).
- `floppy insert --readonly` — per-insertion write-protect tab (now enforced, wired in Phase 1).
- `floppy save-as <new.dmg>` — eject a read-only-source floppy into a fresh writable DMG; template stays pristine.

### Test strategy

- Unit: `attachArgs` emits `-readonly` for the readonly variant; the read-only-source guard refuses in-place write-back and routes to save-as; `Seal`/`Unseal` no-op stubs on `!darwin`.
- `make smoke-floppy` (extends): insert a UDZO template `--readonly` → modify → `floppy save-as new.dmg` → assert template DMG mtime/stamp unchanged and `new.dmg` carries the changes. `chflags uchg` blocks write-back → degrades to conflict/recovery, never silent loss.

### Build-tag / GOOS=linux obligations

`chflags`/`-readonly` attach behind `//go:build darwin` with `!darwin` stubs returning `ErrUnsupported`. Keep new exported API referenced on Linux.

### Effort / risk

**Effort: M. Risk: L.** Mostly mechanical on top of existing convert/attach; the only subtle bit is the "never write back to a read-only source" guard intersecting Phase 2's write-back.

---

## Phase 4 — Finder-eject via DiskArbitration (DEFERRED past v1 — resolves §8.D)

**Recommendation: defer.** Substantial new cgo surface (DiskArbitration.framework + CoreFoundation, `DARegisterDiskUnmountApprovalCallback`, a `CFRunLoop` on a dedicated session thread) — a notable departure from the repo's pure-Go+hdiutil-exec approach. The import/export model already makes the DMG a savefile, so a Finder force-eject mid-session loses only changes-since-last-checkpoint and does **not** crash the running instance — acceptable and documented for v1.

### If/when pursued

- **`internal/floppy/diskarb_darwin.go`** (cgo, darwin-only) + **`internal/floppy/diskarb_other.go`** (`!darwin` stub `ErrUnsupported`). Veto unmount → acquire per-floppy lock → final checkpoint/flush → approve. Started from the checkpoint goroutine's lifecycle in `runStart`.

### Test strategy

Manual + a darwin-only smoke step (cannot run in Linux CI). The `!darwin` stub keeps CI green.

### Effort / risk

**Effort: XL. Risk: H** (cgo + framework linking + run-loop threading + entitlements). Explicitly out of v1.

---

## Phase 5 — optional live VirtioFS data share `share/<name>/` (DEFERRED past v1 — resolves §8.C)

**Recommendation: defer to fast-follow.** Orthogonal to the core insert/checkpoint/eject value; adds per-floppy mount lifecycle. The machine-level VirtioFS share is already wired (`vmconfig_darwin.go configureShare`, `cloudinit.go renderShareSetup`) and `disk.ShareSpec` (`manifest.go:62`) is the reusable shape.

### If/when pursued

- **`internal/floppy/floppy.go`** (edit) — optional `Share *disk.ShareSpec` on `Manifest`; per-floppy mount/unmount tied to insert/eject.
- Per-instance GUI windows come from **Incus** (web-UI console / VNC), one per instance — not VZ's single framebuffer (already true via the existing API path; surface as `floppy console <name>` if desired).

### Effort / risk

**Effort: L–M. Risk: M** (per-floppy mount lifecycle, share-tag collisions across multiple floppies).

---

## Sequencing, parallelization, dependencies

```
Phase 0 (btrfs) ─────────────┐  (parallel: pre-baked image track, gated on artifact-name reconcile)
                             ▼
Phase 1 (DMG fmt + insert + registry)
                             ▼
Phase 2 (checkpoint loop + eject + write-back)   ← load-bearing, highest scrutiny
                             ▼
Phase 3 (write-protect / templates / save-as)
                             ⋮  (deferred, not in v1)
Phase 4 (DiskArbitration)    Phase 5 (live share)
```

- **Critical path:** 0 → 1 → 2 → 3. Phases 4 and 5 are deferred (recommended out of v1).
- **Parallelizable now:** Phase 0's provisioning edits and the pre-baked-image publish/reconcile work are independent of the entire host DMG surface (Phases 1–3) — the host code can be built and tested against a cold-init machine. The "fast UX" milestone (§10.5: seconds-not-7-minutes insert) gates on the published btrfs image but **correctness does not** (§8.F).
- **Within Phase 1:** the `internal/incus` wrapper methods and the `internal/floppy` manifest/registry can be built by two people in parallel; they meet at `runFloppyInsert`.
- **Hard dependencies:** Phase 2 needs Phase 1's registry + import wrapper. Phase 3's save-as branches into Phase 2's eject. Do **not** flip `config.go:280 useHosted=true` until the `-no-agent` artifact naming is reconciled (else download 404s).

## Cross-cutting guardrails (every phase)

- Darwin-only host bits behind `//go:build darwin` with `!darwin` stubs returning `ErrUnsupported`; **GOOS=linux must build clean** (copy the `cartridge_darwin.go`/`cartridge_other.go` split).
- No new heavy deps (UUID via `crypto/rand`+hex, not `google/uuid`). `golangci-lint(latest)` clean: keep new exported API referenced on Linux (unused-code trap), `gocyclo<=25` on orchestration funcs (insert/checkpoint/eject get long), `unparam` on helpers.
- Drive all insert/checkpoint/eject via the **REST SDK** (`internal/incus.Client`) over the existing `127.0.0.1:18443`→vsock→guest `:8443` path with `op.WaitContext(ctx)` — never shell `incus export/import` over SSH.
- Never conflate with VZ `save`/`restore` (whole-VM machine state) — different layer, different verb.
- Prove host behavior live with `make smoke-floppy` (mirroring `scripts/smoke-cartridge.sh` / `Makefile:97`), grown phase-by-phase.

## Success-metric → phase mapping

| Metric (§10) | Proven in |
|---|---|
| 1. Round-trip integrity | Phase 2 smoke |
| 2. Anti-clobber zero-silent-loss | Phase 2 fault-injection |
| 3. Checkpoint ≤4s/GB non-pausing | Phase 0 (btrfs) + Phase 2 smoke |
| 4. Bounded crash loss | Phase 2 (checkpoint cadence) |
| 5. Fast insert UX | Phase 0 pre-baked-image track (gated) |
| 6. Multi-floppy independence | Phase 1 registry + Phase 2 smoke |
| 7. CI/convention health | Every phase (build-tag + lint + smoke obligations) |

**Relevant grounding files:** `internal/cartridge/cartridge.go`, `internal/cartridge/cartridge_{darwin,other}.go`, `internal/incus/instance.go`, `internal/disk/{manifest,catalog}.go`, `internal/control/control.go`, `internal/provision/cloudinit.go`, `scripts/build-guest-image.sh`, `internal/vm/{assets,metadata}.go`, `cmd/bladerunner/{eject,start,incus_client,output,ls,floppy}.go`, `internal/config/config.go`, `scripts/smoke-cartridge.sh`, `Makefile`.