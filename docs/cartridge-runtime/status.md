# Project status: cartridge runtime

> **As of 2026-07-27, `main` @ `7dd2958`.** Written after an independent audit of
> the four merged PRs. This document records what is actually true, including
> where earlier claims (in PR descriptions and in `design.md`) were overstated.
> Architecture of record is [design.md](design.md); the practical guide is
> [usage.md](usage.md). This file is the honest status sheet.

---

## 1. What shipped

| PR | Commit | What |
|---|---|---|
| #176 | `9a3031c` | Standalone cartridge runtime with a per-VM holder |
| #177 | `a059ac7` | Made the cartridge smoke test pass end to end |
| #178 | `88f8eae` | One owner for device names and atomic writes |
| #180 | `7dd2958` | Aligned the local trivy gate with CI |

`main` CI is green: Build & Test, Lint, Security Scan, check.

---

## 2. Status against the five goals — read the caveats

| # | Goal | Status |
|---|---|---|
| 1 | VM held by a minimal wrapper that outlives bladerunner | **partial — see below** |
| 2 | Manage multiple instances concurrently | built, never verified with two VMs |
| 3 | Transportable DMG cartridge | done |
| 4 | Detect a mounted cartridge, offer to boot | done, but nothing detects out of the box |
| 5 | Orderly spin-down on unmount | done, advisory only |

### Goal 1 is the one to be careful about

`br vmd` — the holder — has **exactly one production spawn site**:
`cmd/bladerunner/cartridge_watch.go:526`, reachable only from `br watch` or the
menubar's mount watcher.

Every ordinary path — `br start`, `br up`, `br boot <disk>`, and even
`br boot <cartridge.dmg>` — builds a `vmhost.Host` and runs it **in the
foreground of the CLI process** (`cmd/bladerunner/start.go:114`). Ctrl+C or
closing the terminal still takes the VM with it.

There is a second, older detach path: `startVMDetachedAndWait`
(`cmd/bladerunner/vmgate.go:53`) forks `br start` detached when another verb needs
a VM and none is running. So a VM *can* outlive the invoking command — but via a
detached copy of the full CLI, not via the minimal holder.

**`design.md` §9 used to contradict itself on this.** Its W5 row was correct
("its production caller is the mount watcher; `br start` and `br boot` still run a
`Host` in the foreground"), and then five lines later it asserted "All five goals
in §1 are now met end to end". The second statement was false as written, and
PR #176's summary repeated the wrong version. §9 has since been corrected — see
§5 — and now records goal 1 as partial.

### Goal 2

The mechanism is genuine: `portalloc` returns bound listeners with ephemeral
fallback, and control sockets, lock files, registry entries and ssh fragments are
all per-state-dir. But **no test or smoke ever runs two VMs at once** — both smoke
scripts boot exactly one. One shared global remains: the base-image cache
downloads to a single fixed path with no cross-process lock
(`internal/vm/assets.go:365`), so two instances racing a first download is
untested. That hazard predates this work.

`scripts/smoke-cartridge.sh`'s preflight used to explain itself with
"bladerunner uses fixed ports — one VM at a time". That was stale, and has been
corrected; the port check itself is unchanged and still wanted.

### Goal 4

Detection requires either a foreground `br watch` or the menubar, and the menubar
is installed only by an explicit `br menubar install`. A user who installs
bladerunner and double-clicks a cartridge DMG gets a mounted volume in Finder and
nothing else. `usage.md` and `README.md` condition this correctly; `design.md` §1
and PR #176 stated it unconditionally — `design.md` has since been corrected.

### Goal 5

The veto fires only when **all** of: kind is cartridge, a cartridge is attached,
the dev node reduces to a bare BSD name, the DiskArbitration session opens, and
the watch registers. Every failure **fails open** with a `WARN` — deliberate and
documented, so the VM still runs. Finder's "Force Eject", `diskutil unmount
force`, and direct `umount(2)` all bypass a dissenter regardless.

`Host.UnmountProtection()` records *why* protection is off, but has **zero
production callers** — it is not in `br instances`, `--json`, or `br status`. From
the user's side, an unprotected cartridge is exactly the log line the code comment
claims it is not.

---

## 3. What is actually verified

| Run | pass | fail | skip |
|---|---|---|---|
| darwin `go test -race ./...` | 1368 | 0 | 6 |
| linux/arm64 (`make test-linux`) | 1272 | 0 | 5 |
| `BLADERUNNER_CARTRIDGE_IT=1` gated tests | 3 | 0 | — |
| `make smoke-holder` (real VM) | pass | | |
| `make smoke-cartridge` (real VM) | pass | | |

Lint is 0 issues on darwin **and** `GOOS=linux`. `make security` and CI's trivy
both exit 0. `clonedetect` reports 29 clusters.

### What that does not cover

- **CI has no macOS job.** Every workflow is `ubuntu-*`; the only macOS runner is
  `e2e-boot.yml`, which is `workflow_dispatch` only and non-blocking. **55
  top-level darwin-only tests never run in CI** — all of `internal/vm`, the real
  DiskArbitration suite, `vmhost/unmount_darwin`, and 29 in `cmd/bladerunner`.
  They are verified only by a developer running the suite on a Mac.
- **`vmhost.Host.Run` has 0.0% coverage**, as do all 14 lifecycle steps — 50 of
  82 functions in `host.go`. The headline runtime is exercised only by the two
  smoke scripts, and **neither is wired into any workflow**.
- **67.3%** of the statements this work added or changed were executed
  (2255/3353). `internal/vmhost/host.go` is at 27.4%.
- **`internal/util/atomic_test.go` is vacuous** — the whole suite passes unchanged
  against a plain non-atomic `os.WriteFile`. The atomicity claim is not
  test-verified.
- `make check` runs `go test` **without** `-race`. `make test-linux` is arm64; CI
  is amd64. `tools/clonedetect`'s tests are a separate module and are not in the
  1368.

---

## 4. Behaviour changes users must know

These are not documented in `README.md` or `docs/` and should be.

| Change | Impact |
|---|---|
| **`br stop --force` now cuts power immediately** | Was: graceful ACPI, escalate after 5s. Now: `vz.Stop()` straight away. Scripts using it went from "try clean, then kill" to "kill". |
| **`br stop` default `-t` 30s → 60s** | And it is now a real guest drain budget sent to the server, not just a client-side wait. `config.DefaultStopTimeout` is now an orphaned constant. |
| **`br reset` refuses against a running VM** | New `--force` to override. Breaking for any script that reset while a VM was up. `br reset` appears nowhere in the docs. |
| **`--instance` is accepted and silently ignored by ~half the verbs** | Works: `status`, `stop`, `reset`, `config`, `ls`, `shell`, `ssh`, `eject`. **No-op:** `exec`, `logs`, `events`, `incus`, `reconnect`, `web`, `save`, `restore`, `upgrade`, `menubar`. It is a root persistent flag, so it renders in *their* help with no warning. |
| **Booted cartridges mount under `/Volumes`** | No `--private-mount` flag exists to opt out; the string appears only in a comment. |
| **`br disk pack` names the cartridge after `--out`** | `--out demo.dmg` becomes `demo.dmg.sparseimage` and is then **rejected** by `instance.ValidName`. |
| **ssh config downgrade is lossy** | An older `br` rewrites the aggregator `O_TRUNC`, silently dropping the `Include` line and orphaning every named instance's fragment. |
| **`br boot` on a `.dmg` still discards guest changes** | The working copy is deleted on close. This was *guarded* (it no longer unlinks a still-attached image) but not fixed. |
| Ports are a preference, not a guarantee | Default instance keeps 6022/18443/18444/15556/15557; additional instances take ephemeral. Anything hardcoding a port works only for the default. |

### New persistent files — two outside the state dir

`<stateDir>/instances/<name>.json` · `<stateDir>/control.lock` ·
`<stateDir>/vmd[-<name>].log` · `<cartridge volume>/cartridge.json` ·
`~/.config/bladerunner/ssh/config.d/<name>` · and a hidden
**`.<name>.sparseimage.lock` written next to the user's cartridge image**, i.e.
in `~/Downloads`, not under the state dir.

Backward compatibility is good in one direction: a new `br` reads old cartridges,
settings and boot-stage files. Old `br` reading new files is fine except the ssh
aggregator above.

---

## 5. Doc inaccuracies — all six are now corrected

Every item the audit raised has been fixed. Kept here, resolved rather than
deleted, so the record shows what was wrong and what was done about it.

| # | Was wrong | Resolution |
|---|---|---|
| 1 | `design.md` §9's closing sentence ("All five goals in §1 are now met end to end") contradicted its own W5 row | **Fixed.** §9 now names goals 2–5 as met and goal 1 as **partial**, and repeats that `br start`/`br up`/`br boot` run the `Host` in the CLI foreground while the mount watcher is the only production holder spawn site. |
| 2 | `design.md` §1 goal 4 stated mount detection unconditionally | **Fixed.** Goal 4 is now conditioned on a foreground `br watch` or the installed menubar, matching `usage.md` and `README.md`. |
| 3 | `usage.md` gave the holder log as `vmd.log` for a watcher-booted cartridge | **Fixed.** It is `vmd-<name>.log`; only the flat default keeps `vmd.log` (`vmdLogName`, `cmd/bladerunner/vmd.go`). Asserted by `TestHoldersDoNotShareALogFile`. |
| 4 | A stale, untracked `CLAUDE.md` sat in the main checkout describing `cmd/br-agent`, `--use-guest-agent` and "two boot paths" — all deleted in #166 | **Resolved.** That copy has been removed (archived outside the repo). The tracked `CLAUDE.md` is the only source. |
| 5 | The Makefile help for `smoke-cartridge` said ~5-10 min | **Fixed.** It now says ~15-25min, agreeing with the script header. |
| 6 | `scripts/smoke-cartridge.sh`'s preflight said "bladerunner uses fixed ports — one VM at a time" | **Fixed.** The explanation now says ports are a preference with ephemeral fallback, and why the check is still worth keeping. The check itself is unchanged. |

### Still outstanding

§4's behaviour changes are documented nowhere in `README.md` or `docs/`. That is
a gap in user-facing documentation, not a doc inaccuracy, and is tracked
separately.

---

## 6. Open backlog

**Correctness**
- Two competing lock designs: `internal/control` uses `O_EXCL` + PID with a
  self-admitted residual race; `internal/cartridge` uses `flock`, which has
  neither problem. Converge on `flock`.
- `control.lock` / `control.sock` are written *inside* the cartridge volume, keyed
  on a bare PID, so a crash leaves a stale lock on a `.sparseimage`.

**Leaks** (unbounded first)
- Two ssh `config.d` fragments per cartridge boot, under two different names,
  never removed. The stale alias keeps advertising a port later handed to another
  instance.
- A killed holder strands its attached volume and multi-GB working copy;
  `br stop --force` cleans only the socket.

**Architecture**
- Discovery is fragmented: four `instance.List` walks, two liveness policies, a
  fifth resolution policy in `eject`. The proposed `locate` package was designed
  but not built.
- `clonedetect`'s remaining top clusters: XDG dir resolution copied 5×,
  `processAlive` 2×, `json.MarshalIndent` + `os.WriteFile` 4×.

**Verification**
- Wire `smoke-holder` / `smoke-cartridge` into a macOS workflow, or accept that
  the holder runtime is only ever verified by hand.
- Make `internal/util/atomic_test.go` non-vacuous.
- Run two VMs concurrently, once, to substantiate goal 2.

**Features**
- W10 cartridge persistence (a booted `.dmg`'s working copy is discarded).
- `--private-mount` flag.
- TCC `Info.plist` keys (`NSDownloadsFolderUsageDescription`,
  `NSRemovableVolumesUsageDescription`) — without them the LaunchAgent menubar may
  be denied reading a volume mounted from `~/Downloads`.
- Surface `Host.UnmountProtection()` in `br instances` / `br status`.
- The menubar retains single-VM assumptions outside the new watcher.

**Security**
- 2 HIGH advisories remain in `site/package-lock.json` (astro). Unfixable without
  a **two-major** jump: 5.18.2 is the last 5.x, and `npm audit` flags everything
  `<= 7.0.9`. Currently excluded from the gate, not fixed.
- `GO-2026-5932` (x/crypto openpgp, unmaintained) has no fixed version and never
  will. UNKNOWN severity, below the gate. No ignore file added.

---

## 7. How to verify locally

```
make check          # fmt, vet, lint, test (NOTE: no -race)
go test -race ./... # the real suite
make test-linux     # Linux in Docker — catches build-tag and platform breakage
make security       # same gate as CI
make clonedetect    # ranked duplicate concepts
./scripts/mutation-test.sh

BLADERUNNER_CARTRIDGE_IT=1 go test -race -run Integration ./internal/cartridge/

make smoke-holder      # ~5-15 min, boots a real VM
make smoke-cartridge   # ~15-25 min, boots a real VM
```

The two smoke scripts must **not** be run under an outer timeout shorter than
their budget. Killing the script mid-wait produces a log that reads like a boot
failure when the guest was simply still coming up. Since #177 the log distinguishes
the two: look for `cause="received signal: terminated"` and
`"this is a shutdown, not a boot timeout"`.
