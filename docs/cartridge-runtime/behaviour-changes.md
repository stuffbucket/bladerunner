# Behaviour changes in the cartridge runtime

What changed for a user or a script that already drives `br`. Each entry says
what it was, what it is, and what you have to do about it. The architecture is in
[design.md](design.md); the workflow is in [usage.md](usage.md); the honest
project status is in [status.md](status.md).

Everything below is current behaviour on `main`, checked against the code rather
than against a pull-request summary. Where a change is a **behaviour change** it
says so; where it is a **surprise that was always there** it says that instead.

---

## 1. `br stop --force` now cuts power immediately

**Was:** `--force` asked for a *graceful* shutdown, waited a fixed 5 seconds, and
only then escalated to SIGTERM and SIGKILL on the host process. It was "try
clean, then kill".

**Is:** `--force` skips the ACPI request entirely and cuts power to the guest at
once, then escalates to signals if even that stalls. It is "kill".

The forced stop is `vz.Stop()`, reached the moment the request arrives — the
drain state machine branches on `force` before it presses the ACPI power button.

**What to do:** if a script used `br stop --force` as a safe default, drop the
flag. Plain `br stop` is now the "try clean, then escalate on a budget" path that
`--force` used to be, and it reports when it escalated. Reach for `--force` only
when the guest is genuinely wedged; it is a power cut and it can leave the
cartridge filesystem dirty, which is the one thing the cartridge premise depends
on not happening.

## 2. `br stop` default timeout is 60s, and it is now the guest's budget

**Was:** `-t` / `--timeout` defaulted to 30 seconds (`config.DefaultStopTimeout`)
and bounded only how long the *client* waited.

**Is:** it defaults to 60 seconds (`control.DefaultEjectTimeoutSeconds`) and is
sent to the server as the guest's real drain budget: the guest gets that long to
power itself off, and only when it expires does the host force the stop and say
so. The client then waits the budget plus a teardown margin (15s) on top, because
after the guest powers off the host still has to flush the image, close the
forwarders and exit.

The budget is clamped to 9 minutes so a very large `--timeout` cannot turn into a
control-client transport error.

`config.DefaultStopTimeout` is now an orphaned constant — nothing reads it. If
you were matching on the old 30-second behaviour, stop.

## 3. `br reset` refuses to run against a running VM

**Was:** `br reset` deleted the instance's disk and cloud-init files whatever the
VM was doing.

**Is:** it refuses, with
`"<name>" is running; stop it first with 'br stop' (or reset it anyway with 'br reset --force')`.
The new `--force` / `-f` overrides, following `br stop --force`: same flag, same
shorthand, same "I know, do it anyway" meaning.

This is a guard, not a nicety. The file it deletes is the one the VMM has open;
unlinking it under a live guest loses everything written since boot and leaves
the VM running on an inode nothing can reach.

**Breaking for any script that reset while a VM was up.** Add `br stop` before
it, or add `--force` if the old destructive behaviour was intended (for example,
recovering a wedged instance).

`br reset` also now targets the instance selected by `--instance` rather than
always the default state dir.

## 4. `--instance` is a root flag that only some verbs honour

`--instance` (and `BLADERUNNER_INSTANCE` / `BR_INSTANCE`) is a **persistent flag
on the root command**, so cobra renders it in the help of *every* verb. Only
these eight actually resolve it:

| Verb | How |
|---|---|
| `br status` | `resolveInstanceTarget()` |
| `br stop` | `targetStateDir()` |
| `br reset` | `resolveInstanceTarget()` |
| `br config` | `targetStateDir()` (get, set, keys) |
| `br shell` | `sshTarget()` |
| `br ssh` | `sshTarget()` |
| `br ls` | `incusClientForTarget()` |
| `br eject` | `selectedInstanceName()`, when no name is given positionally |

**Every other verb silently ignores it** and acts on the flat default instance.
The ones where that is a genuine surprise, because they *do* talk to a running
VM:

| Verb | What it actually targets |
|---|---|
| `br exec` | `connectIncus()` → the default state dir |
| `br logs` | `connectIncus()` → the default state dir |
| `br events` | `connectIncus()` → the default state dir |
| `br incus` | `sshConfigFromControl()` → `requireRunningVM()` → the default state dir |
| `br reconnect` | `sshConfigFromControl()` → the default state dir |
| `br web` | `webEndpoints()` → `requireRunningVM()` → the default state dir |
| `br save` | `control.NewClient(config.DefaultStateDir())` |
| `br restore` | `control.NewClient(config.DefaultStateDir())` |
| `br upgrade` | `config.DefaultStateDir()` |
| `br up` / `br start` / `br boot` | the default state dir (via `vmgate`), or the cartridge named on the command line |

The remaining verbs — `br disk`, `br disks`, `br user`, `br notice`,
`br menubar`, `br self-update` — are not instance-scoped at all: they act on the
disk shelf, the identity store, or the host install. `br instances` deliberately
ignores `--instance` because listing everything is its job.

There is **no warning** when a verb ignores the flag. `br exec --instance green`
runs against the default VM and says nothing.

**What to do until this is fixed:** for a verb not in the first table, address the
instance another way — `br ssh --instance green` then run the command in the
guest, or `br shell --instance green`. Do not assume `--instance` reached
`br exec`, `br save`, `br restore` or `br upgrade`.

## 5. Booted cartridges mount browsably under `/Volumes`, with no opt-out

**Was:** a cartridge attached `-nobrowse` at `<state-dir>/mnt/<name>` — invisible
in Finder.

**Is:** the default mount policy is browsable. macOS places the volume at
`/Volumes/bladerunner-<name>` (with a ` 1`-style suffix on a name collision) and
the real mountpoint is read back out of `hdiutil attach -plist` rather than
dictated.

This is deliberate: ejecting the volume is the gesture that asks for an orderly
shutdown, and a volume nobody can see is a volume nobody can eject.

**There is no CLI flag to opt a boot back into the private mount.** The private
policy survives as a value, and `br disk pack` (and `cartridge.Attach`) are pinned
to it so packing and booting never contend for one mountpoint — but no boot path
exposes it. `--private-mount` appears only in source comments and in
`scripts/smoke-cartridge.sh`'s prose; it is not a flag you can pass.

**What to do:** expect a booted cartridge to be visible and ejectable in Finder,
and expect an idle eject click to start a full VM shutdown. That shutdown is
orderly (see §9 of `usage.md`), but it is still a shutdown.

## 6. `br disk pack --out demo.dmg` produces `demo.dmg.sparseimage`, then fails

`--out` is passed through `ensureSparseExt`, which appends `.sparseimage` unless
the path already ends in it. So `--out demo.dmg` becomes `demo.dmg.sparseimage`.

The cartridge name is then derived from that output path by trimming one
cartridge extension, giving `demo.dmg`, which is checked against
`instance.ValidName`. That regex is `^[a-z0-9][a-z0-9-]*$`, so the dot is
rejected and the pack fails before anything is written:

```
cartridge name "demo.dmg" derived from output path demo.dmg.sparseimage is unusable:
invalid instance name: "demo.dmg" must match ^[a-z0-9][a-z0-9-]*$ ...
```

**What to do:** pass `--out demo.sparseimage`, or `--out demo`, or omit `--out`.
Use `--ship` to get the `.dmg`; you do not name it with `--out`. The failure is
loud and happens before any work, so nothing is left behind — but the error names
a path you did not type, which is why it is here.

## 7. Downgrading `br` drops the ssh `Include` line

The ssh config is now an aggregator. `~/.config/bladerunner/ssh/config` holds the
legacy `Host bladerunner` block **plus** an `Include` of
`~/.config/bladerunner/ssh/config.d/*`, and each named instance writes its own
fragment at `config.d/<name>` with a `Host bladerunner-<name>` alias.

The aggregator is written `O_TRUNC` — the whole file is replaced on every write.
A **newer** `br` writes the `Include` line; an **older** `br` writes the same path
without it. So running an older `br` after a newer one silently drops the
`Include`, and every named instance's `config.d/<name>` fragment is orphaned:
still on disk, no longer reachable, so `ssh -F ~/.config/bladerunner/ssh/config
bladerunner-green` stops resolving.

This matters more than it used to, because holders now outlive the CLI: `brew
upgrade` and `br self-update` replace `br` while old holders keep running old
code, and version skew is a standing condition rather than a one-off.

**What to do:** if named-instance ssh aliases stop working after a downgrade or a
mixed-version session, start any instance with the current `br` — that rewrites
the aggregator with the `Include` — or re-add the line by hand. The fragments
themselves are intact.

## 8. `br boot` on a `.dmg` still discards guest changes

A shipped `.dmg` is read-only, so `br boot` converts it to a writable
`.sparseimage` working copy next to the original, boots that, and **removes the
working copy when the cartridge is closed**. Everything the guest wrote goes with
it.

This was *guarded* in the cartridge work, not fixed: the removal now happens only
after the volume is genuinely detached, and a stale working copy that is still
attached is refused rather than unlinked, so the failure mode "delete an image the
kernel is still serving" is gone. The data still does not survive.

`br boot` on a `.sparseimage` attaches it **in place** and does persist.

**What to do:** if you need a cartridge's guest changes to survive, boot the
`.sparseimage` form, not the `.dmg`. Cartridge persistence for the shipped form is
outstanding work (W10 in `design.md`).

## 9. Ports are a preference, not a guarantee

**Was:** the well-known ports were effectively fixed, which is why only one VM
could run at a time.

**Is:** every instance *prefers* `6022` (SSH), `18443` (Incus API), `18444` (web),
`15556` (OIDC) and `15557` (NTP), and any instance that finds a preferred port
already bound falls back to a kernel-assigned ephemeral port rather than failing
to start. Reservation is all-or-nothing across the set, and each port is handed to
its service as an already-**bound listener**, so nothing can steal it in between.

In practice the first instance up keeps the well-known ports and every additional
one gets ephemeral. It is decided by who binds first, not by the instance's name —
so on a host where a second VM started first, the "default" instance is the one on
ephemeral ports.

If reservation fails outright, the services fall back to binding the well-known
ports themselves, exactly as before reservations existed.

**What to do:** stop hardcoding ports for anything but a single-VM install. Read
them from `br instances` (or `br instances --json`, which reports what each
instance actually got), or use the generated ssh config rather than `-p 6022`.
`OIDCIssuerURL` and the other derived URLs are re-derived from the port actually
reserved, so they follow automatically.

## 10. New files on disk, two of them outside the state dir

| Path | What |
|---|---|
| `<stateDir>/instances/<name>.json` | The instance registry entry, published by the holder and removed on clean exit. Pruned by `br instances` when the holder is gone. |
| `<stateDir>/control.lock` | The ownership claim taken next to `control.sock` before the dial/bind dance. |
| `<stateDir>/vmd.log`, `<stateDir>/vmd-<name>.log` | The detached holder's raw stdout/stderr, one per instance. Rotated at 10 MB, 3 backups, 14 days. |
| `<cartridge volume>/cartridge.json` | The cartridge's self-description and format stamp — inside the mounted volume, so it travels with the image. |
| `~/.config/bladerunner/ssh/config.d/<name>` | The per-instance ssh fragment (see §7). |

And one that is easy to miss:

**`.<name>.sparseimage.lock`, written next to your cartridge image.** Not under
the state dir — next to the file, so for an AirDropped cartridge that is
`~/Downloads/.demo.sparseimage.lock`. It is a hidden sibling on purpose: the claim
has to live on the same filesystem as the thing it protects, and a cartridge can
be booted from anywhere, including a removable volume.

**The lock file is deliberately left behind when the claim is released.** Only its
contents are blanked. Unlinking it would let a second process create and lock a
different inode for the same path while the first still believed it held the
claim. So a directory you have booted cartridges from accumulates one small hidden
file per cartridge. They are safe to delete when nothing is running, and safe to
leave.

---

## Compatibility, in both directions

A **new** `br` reads old cartridges, old settings and old boot-stage files.

An **old** `br` reading new files is fine with one exception: the ssh aggregator
in §7.
