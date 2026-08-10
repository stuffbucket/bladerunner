# Behaviour changes in the cartridge runtime

What changed for a user or a script that already drives `br`. Each entry says
what it was, what it is, and what you have to do about it. The architecture is in
[design.md](design.md); the workflow is in [usage.md](usage.md); what is still
open is in [status.md](status.md).

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

**Was:** `-t` / `--timeout` defaulted to 30 seconds and bounded only how long the
*client* waited.

**Is:** it defaults to 60 seconds (`control.DefaultEjectTimeoutSeconds`) and is
sent to the server as the guest's real drain budget: the guest gets that long to
power itself off, and only when it expires does the host force the stop and say
so. The client then waits the budget plus a teardown margin (15s) on top, because
after the guest powers off the host still has to flush the image, close the
forwarders and exit.

The budget is clamped to 9 minutes so a very large `--timeout` cannot turn into a
control-client transport error.

If you were matching on the old 30-second behaviour, stop.

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

## 4. `--instance` is refused by the verbs that do not act on one VM

`--instance` (and `BLADERUNNER_INSTANCE` / `BR_INSTANCE`) is a **persistent flag
on the root command**, so cobra renders it in the help of *every* verb. It used
to be silently ignored by about half of them, which is worse than rejecting it:
the user reads the help, uses the flag, and gets an answer about a different VM.

**Is:** every command declares what it does with the flag, and an undeclared
command refuses it. There are three policies:

| Policy | Commands |
|---|---|
| **Honoured** — resolves the flag and acts on that instance | `status`, `stop`, `restart`, `reset`, `eject`, `save`, `restore`, `upgrade`, `reconnect`, `ssh config`, `shell`, `exec`, `incus`, `ls`, `logs`, `events`, `web`, `config` |
| **All instances** — spans every instance, so selecting one would mean nothing | `instances` |
| **Refused** — does not act on a selected VM | everything else, including `up`, `start`, `boot`, `watch`, `disk`, `disks`, `user`, `notice`, `menubar`, `self-update`, `web untrust` |

A refusal names what to reach for instead, for example:

```
--instance selects a VM to act on; 'br boot' does not act on one
  'br boot' names the instance it creates in its own argument: 'br boot <name>'
```

Only the **flag** triggers a refusal. A `BLADERUNNER_INSTANCE` left in the
environment is a standing preference, not a claim about this invocation, so it
never makes `br disks` fail.

**What to do:** if a script relied on `--instance` being tolerated by a verb that
does not act on one VM, drop the flag there. If it relied on `--instance` being
*ignored* by `br exec`, `br save`, `br restore` or `br upgrade` — those now
honour it, and will act on the named instance rather than the default.

## 5. Booted cartridges mount browsably under `/Volumes` by default

**Was:** a cartridge attached `-nobrowse` at `<state-dir>/mnt/<name>` — invisible
in Finder.

**Is:** the default mount policy is browsable. macOS places the volume at
`/Volumes/bladerunner-<name>` (with a ` 1`-style suffix on a name collision) and
the real mountpoint is read back out of `hdiutil attach -plist` rather than
dictated.

This is deliberate: ejecting the volume is the gesture that asks for an orderly
shutdown, and a volume nobody can see is a volume nobody can eject.

`br boot --private-mount` opts one boot back into the old dictated, invisible
mount — deterministic, and what a script usually wants. `br disk pack` (and
`cartridge.Attach`) are pinned to the private policy unconditionally, so packing
and booting never contend for one mountpoint.

**What to do:** expect a booted cartridge to be visible and ejectable in Finder
unless you pass `--private-mount`, and expect an idle eject click to start a full
VM shutdown. That shutdown is orderly (see *Eject safely* in
[usage.md](usage.md)), but it is still a shutdown.

## 6. `br disk pack --out demo.dmg` is refused up front

`--out` names the **runnable** cartridge form. A path with any other extension is
refused before anything is written:

```
cartridge output path must name the runnable form: demo.dmg ends in ".dmg";
write the runnable cartridge with '--out demo.sparseimage', and add --ship to
also produce the compressed .dmg AirDrop artifact
```

This used to be a much worse failure: `--out demo.dmg` silently became
`demo.dmg.sparseimage`, whose derived cartridge name `demo.dmg` then failed
`instance.ValidName` three calls later and put a regex in front of a user who had
only picked the wrong extension — after `--ship` had advertised a `.dmg` to them.

**What to do:** pass `--out demo.sparseimage`, or a bare `--out demo`, or omit
`--out`. Use `--ship` to get the `.dmg`; you do not name it with `--out`.

## 7. Downgrading `br` drops the ssh `Include` line

The ssh config is now an aggregator. `~/.config/bladerunner/ssh/config` holds the
legacy `Host bladerunner` block **plus** an `Include` of
`~/.config/bladerunner/ssh/config.d/*`, and each named instance writes its own
fragment at `config.d/<name>` with a `Host bladerunner-<name>` alias.

The current `br` *appends* the `Include` to an aggregator that predates
per-instance configs, and publishes both the fragment and the aggregator
atomically, so the existing default-instance block stays first and keeps winning.
An **older** `br` knows nothing about the line and rewrites the whole aggregator
without it. So running an older `br` after a newer one silently drops the
`Include`, and every named instance's `config.d/<name>` fragment is orphaned:
still on disk, no longer reachable, so `ssh -F ~/.config/bladerunner/ssh/config
bladerunner-green` stops resolving.

This matters more than it used to, because holders now outlive the CLI: `brew
upgrade` and `br self-update` replace `br` while old holders keep running old
code, and version skew is a standing condition rather than a one-off.

**What to do:** if named-instance ssh aliases stop working after a downgrade or a
mixed-version session, start any instance with the current `br` — that restores
the `Include` — or re-add the line by hand. The fragments themselves are intact.

## 8. `br boot` on a `.dmg` discards guest changes unless you pass `--persist`

A shipped `.dmg` is read-only, so `br boot` converts it to a writable
`.sparseimage` working copy next to the original, boots that, and **removes the
working copy when the cartridge is closed**. Everything the guest wrote goes with
it. That is still the default and it does not change: a `.dmg` boot is a
throwaway run.

`br boot <file>.dmg --persist` writes the changes back. It never writes into the
original — see the `--persist` description in [usage.md](usage.md) §3 for what it
does instead, and what it leaves behind if the write-back is interrupted.

`br boot` on a `.sparseimage` attaches it **in place** and always persists, so
`--persist` is a no-op there and says so.

**What to do:** a `.dmg` boot that you expect to keep its changes needs
`--persist`. Without it, nothing warns you — the discard is the documented
default.

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
| `<stateDir>/vmd.log`, `<stateDir>/vmd-<name>.log` | The detached holder's raw stdout/stderr, one per instance. Rotated at 10 MB, 3 backups, 14 days. Which file belongs to which instance is in [usage.md](usage.md), *Where the logs are*. |
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
