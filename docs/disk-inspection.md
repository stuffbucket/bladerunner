# Inspecting a guest disk offline (without booting the VM)

When a guest won't boot, `br shell` can't connect, or you suspect filesystem or
provisioning damage, you can read the guest's root filesystem directly from the
host — no VM, no SSH — using read-only `ext4` tooling.

This is how the "guest boots but `br shell` resets with errno 54" case was first
root-caused (the cloud-init bootstrap had failed before creating the vsock SSH
bridge unit).

## Tooling and licensing

macOS has no native `ext4` support, so install `e2fsprogs` (provides `debugfs`,
`dumpe2fs`, `e2fsck`):

```bash
brew install e2fsprogs        # keg-only; binaries under /opt/homebrew/opt/e2fsprogs/sbin
```

`e2fsprogs` is GPL-2.0 (the libraries are LGPL-2.0). Bladerunner is MIT-licensed.
Invoking `debugfs`/`dumpe2fs` as **separate executables** (as below, or from a
future `br inspect-disk` subcommand that shells out to them) does **not** create
a derivative work and imposes no licensing obligation on `runner`. Only *bundling /
redistributing* the GPL binaries inside a `runner` release would trigger GPL source
obligations — so we shell out to a user-installed copy instead of vendoring it.

## Procedure

> Stop the VM first. Reading a live, read-write disk gives inconsistent results,
> and the image file may be held open by the running VZ process.

```bash
runner stop                 # or: runner stop --force   (panicked/hung guest)

cd ~/.local/state/bladerunner

# Attach the raw disk image WITHOUT mounting (macOS can't mount ext4 anyway).
hdiutil attach -nomount -readonly \
  -imagekey diskimage-class=CRawDiskImage disk.raw
# -> /dev/diskN (whole disk), /dev/diskNs1 (Linux root), /dev/diskNsNN (EFI)

diskutil list /dev/diskN            # confirm which slice is the Linux root

export PATH="/opt/homebrew/opt/e2fsprogs/sbin:$PATH"
DEV=/dev/diskNs1                    # the Linux-root partition from diskutil list
```

Useful read-only queries (`debugfs` opens read-only by default):

```bash
# Filesystem health / last fsck / error state
dumpe2fs -h "$DEV" | grep -iE 'Filesystem state|Errors behavior|Last checked'

# Did provisioning create + enable the vsock bridges?
debugfs -R "stat /etc/systemd/system/bladerunner-vsock-ssh.service" "$DEV"
debugfs -R "ls -l /etc/systemd/system/multi-user.target.wants" "$DEV"

# Why did cloud-init / the bootstrap fail?
debugfs -R "cat /var/log/cloud-init-output.log" "$DEV" | tail -40
debugfs -R "cat /var/log/cloud-init.log"        "$DEV" | grep -iE 'fail|error'
debugfs -R "ls -l /var/lib/bladerunner"          "$DEV"   # ready / vsock-diag.txt?

# Inspect a specific inode (e.g. one ext4 flagged with a checksum error)
debugfs -R "stat <1533>" "$DEV"
```

Always detach when done:

```bash
hdiutil detach /dev/diskN
```

## Possible follow-up: a `br inspect-disk` subcommand

The steps above are mechanical and could be wrapped as `br inspect-disk`
(detect `debugfs`/`dumpe2fs`, refuse if the VM is running, auto-pick the Linux
root slice, run a fixed health/provisioning report, always detach). It would
shell out to the user-installed `e2fsprogs` — same licensing position as above.
Not yet implemented.
