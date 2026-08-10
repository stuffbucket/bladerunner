# Known gaps: cartridge runtime

> What the cartridge runtime does **not** do, and what is still open. The
> architecture is [design.md](design.md), the practical guide is
> [usage.md](usage.md), and what changed for an existing script is
> [behaviour-changes.md](behaviour-changes.md).
>
> Limitations a *user* will hit are in [usage.md](usage.md) *Limitations* — this
> page is the engineering backlog behind them. Verification commands are AGENTS.md
> §2; owner packages are AGENTS.md §3.

---

## Correctness

- **Two lock designs are still in the tree.** `internal/control` claims ownership
  with an `O_EXCL` lock file keyed on a bare PID, which has a self-admitted
  residual race; `internal/cartridge` and `internal/vm`'s image cache use
  `flock`, which has neither problem. AGENTS.md §3 says `flock` is the rule.
  Converging `internal/control` onto it is outstanding.
- **`control.lock` and `control.sock` are written inside the cartridge volume**,
  because the mountpoint *is* the state dir for a cartridge. A crash therefore
  leaves a stale lock on the `.sparseimage` itself, which travels with it.

## Leaks

- **ssh `config.d` fragments are never removed.** Nothing deletes an instance's
  fragment when it ejects, so a stale alias keeps advertising a port that may
  later be handed to a different instance.
- **A killed holder strands its attached volume and its multi-GB working copy.**
  `br stop --force` cleans up the socket, not the mount. Recovery is the manual
  `hdiutil detach` in [usage.md](usage.md).

## Architecture

- **Instance discovery is fragmented** — several `instance.List` walks and more
  than one resolution policy (`eject` has its own). A `locate` package to own it
  was designed but not built.
- **The menubar is not a multi-VM UI.** Its read path now resolves through the
  same policy as the CLI, and it displays "ambiguous" instead of silently
  reporting the wrong VM, but it still presents one instance at a time.

## Verification

- **No macOS test job runs in CI, by policy.** Every test workflow is
  `ubuntu-*`; `macos-build.yml` only dispatches a signed release build to
  `stuffbucket/macos-builder`, and `e2e-boot.yml` is `workflow_dispatch` and
  non-blocking. AGENTS.md §4.7 forbids adding a GitHub-hosted macOS runner, so
  the darwin-only tests — `internal/vm`, the real DiskArbitration suite,
  `vmhost/unmount_darwin`, and much of `cmd/bladerunner` — are verified only by a
  developer running the suite on a Mac. Closing this means self-hosted capacity,
  not a workflow change.
- **`make smoke-holder` and `make smoke-cartridge` are wired into no workflow**
  for the same reason. They boot real VMs and are the only end-to-end exercise of
  `vmhost.Host.Run` and its lifecycle steps, which unit tests barely cover.

## Security

- **2 HIGH advisories in `site/package-lock.json` (astro).** `site/` pins
  `^5.18.2`; 5.18.2 is the last 5.x and `npm audit` flags everything `<= 7.0.9`,
  so clearing them needs a two-major jump. Currently excluded from the gate, not
  fixed.
- **`GO-2026-5932`** (`x/crypto` openpgp, unmaintained) has no fixed version and
  never will. UNKNOWN severity, below the gate; no ignore file added.

---

## Running the smoke tests

The two smoke scripts must **not** be run under an outer timeout shorter than
their budget (`smoke-holder` ~5-15 min, `smoke-cartridge` ~15-25 min). Killing
the script mid-wait produces a log that reads like a boot failure when the guest
was simply still coming up. The log distinguishes the two: look for
`cause="received signal: terminated"` and
`"this is a shutdown, not a boot timeout"`.

The cartridge integration tests are gated behind an environment variable:

```
BLADERUNNER_CARTRIDGE_IT=1 go test -race -run Integration ./internal/cartridge/
```
