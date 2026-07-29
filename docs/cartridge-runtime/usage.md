# Using the cartridge runtime

The practical guide: pack a whole VM into one file, send it to another Mac, boot
it, run several at once, and put them away without corrupting anything.

The architecture behind this is in [design.md](design.md). This page is the part
you type. If you already drive `br` from a script or from memory, read
[behaviour-changes.md](behaviour-changes.md) first — several defaults changed,
and `br stop --force` and `br reset` changed in ways that break scripts.

---

## What you need

- Apple Silicon, macOS 13+, and a `br` codesigned with `vz.entitlements`
  (Homebrew and the `.dmg` do this for you; from source it is `make sign`).
- `qemu-img` — only for **packing** a cartridge, not for booting one.
- `hdiutil` — ships with macOS.

---

## 1. Pack a cartridge

A cartridge is one APFS sparse image holding a complete bootable VM: the disk
manifest (`disk.json`), the root disk (`root.img`), EFI + cloud-init state under
`state/`, and a read-write host↔guest folder at `share/`.

```bash
br disks                      # what is on the shelf
br disk pack incus            # -> ./incus.sparseimage (runnable)
br disk pack incus --ship     # also -> ./incus.dmg (compressed, read-only, AirDrop-able)
```

| Flag | Meaning |
|---|---|
| `--out <file>` | Output path (default `./<name>.sparseimage`) |
| `--ship` | Also produce the compressed read-only `.dmg` |
| `--arch <arch>` | Architecture of the root image (default: this Mac's) |
| `--size <GiB>` | Cartridge capacity (default: the disk's size plus headroom) |

Packing downloads and materializes the base image, so the first pack of a given
disk is slow and every later one is fast.

**A cartridge is named after its own file, not after the disk it came from.**
`br disk pack incus --out blue.sparseimage` produces a volume named
`bladerunner-blue`, stamps `blue` into the cartridge, and `br boot
blue.sparseimage` runs it as instance `blue`. That is what lets several
cartridges packed from one base disk mount and run side by side (§4) instead of
all claiming `/Volumes/bladerunner-incus`. With `--out` omitted the output is
`./<disk>.sparseimage`, so the two names coincide. The name must be lowercase
letters, digits and dashes — an `--out` whose base name is not is refused before
anything is written.

## 2. Ship it

AirDrop, `scp`, a USB stick — it is one file. Ship the `.dmg` (`--ship`): it is
compressed and read-only, so the artifact you send stays byte-identical no
matter how many times it is booted.

This is safe because `br eject` always powers the guest off cleanly via ACPI and
waits for it to actually stop, so a packed cartridge is always in a consistent
**cold-boot** state. There is no host-bound RAM snapshot travelling with it, and
that is deliberate: the disk is portable, same-host RAM resume (`br save` /
`br restore`) is not.

## 3. Insert it

Double-clicking a `.dmg` in Finder mounts a read-only view of it. That is the
gesture the runtime watches for:

```bash
br watch          # notice cartridges being inserted and ask before booting each
br watch --yes    # boot every cartridge found, no questions (alias: --auto)
br watch --once   # report what is mounted right now and exit
br watch --json   # one JSON record per event; reports, never boots unasked
```

The menubar runs the same watcher, so if it is running you get the offer as a
notification and do not need `br watch` at all.

What it does per appearing volume, in this order: reject anything that is not
named like a cartridge (cheap — this callback fires for every USB stick on the
machine); ignore a volume some instance is *already* holding, because a booted
cartridge's own mount looks exactly like a fresh insertion; then actually read
the volume and classify it. A cartridge that is real but unbootable — no
`root.img`, packed by a newer bladerunner, unreadable because of macOS privacy
permissions — is reported **with the reason**, not silently skipped.

On accept, the read-only view is unmounted and a holder is started against the
image **file**, not the mounted view, so the artifact you were sent stays
pristine.

You never need the watcher. Booting by path always works, and is the only option
for a cartridge that macOS cannot trace back to its file:

```bash
br boot ~/Downloads/incus.dmg          # converts to a working copy, then cold-boots it
br boot ~/Downloads/incus.sparseimage  # boots the runnable form directly
```

`br boot` on a `.dmg` first converts it to a writable `.sparseimage` next to the
original. It then attaches the image **browsably** — macOS places the volume at
`/Volumes/bladerunner-<name>` and appends a suffix if that name is taken — roots
the VM inside the mount (`root.img`, state under `state/`, the RW share at
`share/`) and owns that mount for as long as the VM runs.

Browsable is deliberate: a volume nobody can see is a volume nobody can eject,
and ejecting is how you ask for an orderly shutdown (§5). Packing still attaches
privately under the state dir, so `br disk pack` and `br boot` never contend for
one mountpoint.

The host side of the share is `share/` inside the mount; the guest sees it at
`/mnt/share` over VirtioFS. Drop a file in either and it appears in the other.

## 4. Run several at once

Ports are per-instance now. The flat default instance still prefers the
well-known ports (`6022`, `18443`, `18444`, `15556`, `15557`) so existing docs,
scripts and hand-written ssh configs keep working; any instance that finds those
taken gets ephemeral ports instead of failing to start.

```bash
br boot ./blue.sparseimage      # terminal 1
br boot ./green.sparseimage     # terminal 2
br instances                    # what is running, on which ports, held by which PID
```

```
NAME   KIND       SSH    API    UPTIME  PID    STATE DIR                  SOURCE
blue   cartridge  6022   18443  4m12s   41207  /Volumes/bladerunner-blue  /…/blue.sparseimage
green  cartridge  53812  53813  1m02s   41880  /Volumes/bladerunner-green /…/green.sparseimage
```

Pick one for any verb with `--instance` (or `BLADERUNNER_INSTANCE` /
`BR_INSTANCE` in the environment):

```bash
br status  --instance green
br shell   --instance green
br ssh     --instance green
br stop    --instance green
br eject   green
```

The selection policy is deliberately boring:

- an explicitly named instance always wins, running or not;
- with exactly **one** running instance, `--instance` may be omitted — this is
  every single-VM install and nothing changed for it;
- with **none** running, verbs resolve to the flat default layout and report
  "not running", exactly as they always did;
- with **more than one** running and no name, you get an error that lists the
  candidates and their ports rather than a coin flip.

Each instance also writes its own ssh config fragment under
`~/.config/bladerunner/ssh/config.d/<name>`, pulled in by an `Include` from the
aggregator at `~/.config/bladerunner/ssh/config`. The legacy `Host bladerunner`
block is still there, so `ssh -F ~/.config/bladerunner/ssh/config bladerunner`
keeps working.

## 5. Eject safely

Every shutdown path is now an orderly drain: ask the guest to power itself off
over ACPI, **wait for it to actually reach the stopped state**, and escalate to
a forced stop only when the budget expires — reporting that it did.

```bash
br eject <name>                 # power off the guest, then detach the cartridge
br stop --instance <name>       # same drain, without the cartridge framing
br stop --timeout 120           # give a busy guest longer to shut down
br stop --force                 # power cut: for a panicked guest that ignores ACPI
```

On darwin the holder also registers a DiskArbitration **unmount-approval**
callback against its cartridge's BSD device node. So if you ask macOS to eject
the volume — Finder, `diskutil unmount`, anything that goes through DiskArb —
what happens is:

1. The request is refused with `kDAReturnBusy` and the reason
   *"bladerunner is shutting down the VM on this cartridge"*, which Finder shows
   verbatim in its "could not eject" dialog.
2. An orderly drain starts in the background. The refusal is immediate; the
   callback never blocks for the drain budget.
3. Once the guest is genuinely stopped, teardown detaches the volume itself.
   Your second eject click (or nothing at all) finds it already gone.

A holder started detached also treats **SIGTERM** as "eject in an orderly way",
not as "die": the first SIGTERM starts the drain, and only a second one
escalates to a power cut.

---

## When something goes wrong

**"multiple instances running; select one with --instance"** — exactly what it
says. The error lists every candidate with its kind and ports; pick one.

**A verb cannot find an instance you know is running.** `br instances` unions the
registry with a scan of the legacy layout and, as a side effect, prunes entries
whose holder is gone. Run it first. The registry lives at
`~/.local/state/bladerunner/instances/<name>.json` and is advisory — the
authoritative answer is always whether the control socket at
`<state-dir>/control.sock` answers.

**The guest will not power off.** `br stop --timeout <seconds>` widens the drain
budget for a guest with a lot to flush. `br stop --force` skips straight to the
power cut and, if even that stalls, terminates the holder process. Reach for
`--force` only when the guest is genuinely wedged: it is a power cut and it can
leave the cartridge's filesystem dirty, which is the one thing the cartridge
premise depends on not happening.

**Finder says the volume is busy.** That is the veto working. Wait — the drain it
started will unmount the volume for you. If you are impatient, `br eject <name>`
does the same thing with a progress indication.

**A cartridge is still attached after everything exited.** Belt and braces:

```bash
hdiutil info | grep bladerunner
hdiutil detach /path/to/mountpoint -force
```

**Where the logs are.** The instance's own logs live under its state dir —
which for a cartridge is the mountpoint, `/Volumes/bladerunner-<name>`:
`bladerunner.log` for the host side, `console.log` for the guest serial console.
A holder spawned detached also writes its raw stdout/stderr to a holder log in
the directory it was *pointed at*. That log is named **per instance**, because
every cartridge holder is spawned with the registry root as its state dir and
would otherwise interleave into one shared file: a watcher-booted cartridge
`demo` writes `~/.local/state/bladerunner/vmd-demo.log`, and only the flat
default instance keeps the historical `~/.local/state/bladerunner/vmd.log`. A
holder you ran by hand writes to your terminal instead. (`br logs` is unrelated —
it streams logs from an *Incus* instance inside the guest.)

---

## Limitations

These are real, current, and worth knowing before you rely on any of this.

**The unmount veto is advisory.** `DADissenter` is a request, not a lock.
Finder's **Force Eject**, `diskutil unmount force`, `DADiskUnmount` with
`kDADiskUnmountOptionForce`, and a direct `umount(2)` all bypass registered
dissenters — the last one never consults DiskArbitration at all. The veto
narrows the corruption window; it does not close it. If you force-eject a
cartridge with a running guest you will get exactly the dirty filesystem you
asked for.

**Version skew is permanent now.** A holder outlives the CLI, so `brew upgrade`
(or `br self-update`) replaces `br` while old holders keep running old code.
That is not a bug to be fixed once; it is a standing operating condition. The
control protocol negotiates a version, and a newer manager is expected to
degrade gracefully against an older holder — but if a verb behaves oddly against
a long-running VM, check `br instances --json` (each entry carries the
`binary_version` its holder was started from) before assuming the verb is
broken. Draining and restarting the instance puts it on the new code.

**There is no admission control.** N cartridges booted means N VZ VMs and N
guests' worth of committed RAM and disk. Nothing counts them and nothing refuses
the fourth one. AirDropping four cartridges to a 16 GB Mac and booting them all
will wedge that Mac. Size the guests (`--memory`, or the disk manifest) and
count them yourself.

**One holder per cartridge, enforced only by the control socket.** The mutual
exclusion is the bound socket inside the mount (taken under an `O_EXCL` lock)
plus a liveness probe, and the watcher additionally skips a volume some instance
already holds. Two *different* processes attaching the same image file is still
not something the runtime prevents in every ordering — do not boot the same file
twice concurrently.

**Detection can be blocked by macOS privacy settings.** AirDropped cartridges
land in `~/Downloads`, and the menubar runs from a LaunchAgent with no
user-initiated open, so the watcher can find a volume it is not allowed to read.
That case is reported explicitly ("could not be read (permission denied)")
rather than passed over — grant Files and Folders access for Downloads and
removable volumes in System Settings › Privacy & Security, or boot by path with
`br boot <file>`, which needs no such permission.

**Booted cartridges are visible in Finder now.** That is the point (you cannot
eject what you cannot see), but it also means an idle click can start a full VM
shutdown. There is no CLI flag to opt a boot back into the old invisible
`-nobrowse` mount; the private policy is used internally by `br disk pack` only.

**`--instance` is accepted by every verb but honoured by only some of them.** It
is a persistent flag on the root command, so cobra renders it in every verb's
help. `status`, `stop`, `reset`, `config`, `shell`, `ssh`, `ls` and `eject`
resolve it; `exec`, `logs`, `events`, `incus`, `reconnect`, `web`, `save`,
`restore` and `upgrade` silently act on the default instance instead, with no
warning. The full map is in [behaviour-changes.md](behaviour-changes.md) §4.
