# bladerunner

`bladerunner` is a standalone Incus VM runner for macOS built directly on Apple Virtualization.framework via `github.com/Code-Hex/vz/v3`.

It is designed to provide the core behavior of a `colima --runtime incus` setup without Lima/Colima orchestration overhead:

- Architecture-aware defaults (`arm64` and `amd64`). Fresh installs boot the pre-baked bladerunner guest image (Debian 13 trixie + Incus, no first-boot apt); the Debian genericcloud image is the warned auto-fallback and the `--debian-image` escape hatch. Ubuntu and other cloud images remain reachable via `--image-url` or `BLADERUNNER_BASE_IMAGE_URL`.
- Incus daemon shipped in the pre-baked image (or bootstrapped via cloud-init on the Debian fallback path).
- Localhost-accessible SSH and Incus HTTPS endpoints via virtio-vsock port forwarding.
- Incus web dashboard availability through the forwarded API endpoint.
- Optional bridged networking (for transparent L2 presence) when signed with `com.apple.vm.networking`.
- Startup report generation with VM, network, and access details.
- Optional GUI console window (`StartGraphicApplication`) with serial output logged to file.
- Rotating structured logs with stage-level observability and live progress indicators for long-running tasks.
- No OpenID setup by default.

## Requirements

- **Apple Silicon Mac** (M1/M2/M3/M4) - Intel Macs not supported
- macOS 13+ (Ventura or later)
- Xcode Command Line Tools (includes codesign utility)

  ```bash
  xcode-select --install
  ```

- Binary must be code-signed with Virtualization entitlement (automatic with Homebrew)
- For bridged networking, additional VM networking entitlement required

## Installation

### Homebrew (Recommended)

```bash
brew install stuffbucket/tap/bladerunner
```

The binary is automatically signed with required entitlements during installation.

Homebrew installs update via `brew upgrade bladerunner`.

### Download (`.dmg`)

Every release also ships a signed, notarized `.dmg` installer on the
[Releases page](https://github.com/stuffbucket/bladerunner/releases), named
`bladerunner_<version>_darwin_aarch64.dmg` (with a matching `.sha256`).

1. Download the `.dmg` from the latest release.
2. (Optional) verify it:

   ```bash
   shasum -a 256 -c bladerunner_<version>_darwin_aarch64.dmg.sha256
   ```

3. Open the `.dmg` and drag **Bladerunner.app** to `/Applications`.

The bundle is code-signed with the Virtualization entitlement and notarized, so
Gatekeeper allows it on first launch — no `xattr` dance required.

DMG installs self-update in place:

```bash
br self-update          # download + verify + install the latest signed .app
br self-update --check   # just report whether a newer version is available
```

`br self-update` verifies the new bundle's Ed25519 signature before replacing
anything and refuses to run on Homebrew-managed installs (use `brew upgrade`
for those). It is distinct from `br upgrade`, which hands the *running* control
server to a new binary already on disk.

### Build from Source

Requires Xcode Command Line Tools:

```bash
xcode-select --install
```

Build and sign:

```bash
make build
make sign
```

Or manually:

```bash
go build -o bin/br ./cmd/bladerunner
codesign --entitlements vz.entitlements -s - bin/br
```

## Run

Default (shared network + localhost forwarding):

```bash
runner start
```

With GUI console window:

```bash
runner start --gui
```

Bridged network on `en0`:

```bash
runner start --network-mode bridged --bridge-interface en0
```

Custom image path (raw disk image):

```bash
runner start --image-path /path/to/base.raw
```

Custom log file path:

```bash
runner start --log-path /tmp/bladerunner.log
```

Optional log level. Accepts `debug`, `info`, `warn` (alias `warning`), or
`error` (case-insensitive). Unknown or unset values default to `info`:

```bash
BLADERUNNER_LOG_LEVEL=debug runner start
```

## Access

After startup, the tool prints a report and writes JSON report data to:

- `~/.local/state/bladerunner/startup-report.json`

Key defaults:

- Incus API/UI endpoint: `https://127.0.0.1:18443`
- SSH endpoint: `127.0.0.1:6022`
- Dashboard URL: `https://127.0.0.1:18443/ui/`
- Log file: `~/.local/state/bladerunner/bladerunner.log` (rotated with compression)

Example SSH:

```bash
ssh -p 6022 incus@127.0.0.1
```

Example REST call:

```bash
curl --cert ~/.local/state/bladerunner/client.crt --key ~/.local/state/bladerunner/client.key -k https://127.0.0.1:18443/1.0
```

## Disks

A *disk* is a `.disk` JSON manifest that bundles an image identity, VM sizing
recommendations, and a boot mode (headless or GUI) — think of it as a labeled
floppy you slide in and power on. Booting a disk materializes its image, applies
sizing, and runs the VM in an isolated per-disk state slot, restoring saved guest
RAM when present. A disk that pins its image SHA-256 (e.g. after `br disk
bake`) is materialized once into a shared content-addressed cache and reused
across slots; the digest is verified before use.

```bash
runner disks                 # list the shelf (builtins + your disks) and attached cartridges
runner boot <name|url|path>  # power on a disk (restores saved RAM if present)
runner eject                 # cleanly power off the running VM (ACPI shutdown)
runner disk new <name>       # scaffold a new user disk manifest
runner disk bake <name>      # build its qcow2 and record the image SHA-256
runner disk pack <name>      # pack a disk into an AirDrop-able cartridge
```

`br eject` performs a clean ACPI shutdown (it loops the power button and
waits for the guest to power off, then forces the stop after `--timeout`). For a
same-host RAM resume use `br save` + `br restore` instead — eject is a
clean cold-stop by design.

Two disks ship built in:

- **`incus`** — headless Incus host using the pre-baked bladerunner guest image
  (the `guest-image-latest` release; this is the classic `br start` setup).
- **`debian-trixie-gui`** — a Debian Trixie desktop that opens in a VZ window.

`br boot <name>` resolves a catalog disk; `br boot <url>` boots a one-off
headless image by URL; `br boot ./my.disk` boots a manifest file directly.
`--cpus`/`--memory`/`--disk` override the manifest's sizing, and
`--gui`/`--headless` override its boot mode. `--no-restore` forces a cold boot.

Layout:

- User disks: `~/.config/bladerunner/disks/*.disk`
- Per-disk state slots: `~/.local/state/bladerunner/disks/<name>/` (each slot has
  its own `disk.raw`, `saved-state.bin`, console log, EFI vars, and cloud-init)
- Shared image cache (SHA-256-pinned disks only): `~/.local/state/bladerunner/cache/images/<sha256>.raw`

`br disk bake` shells out to `scripts/build-guest-image.sh` and is a
host-side developer action: it requires `bash`, `qemu-img`, and the script's
build dependencies (`libguestfs-tools`, likely `sudo`). Builtin disks are
read-only — fork one with `br disk new <name> --from <builtin>` first.

## Cartridges

A *cartridge* is a single, self-contained, AirDrop-able macOS disk image holding
a complete bootable VM: the disk manifest, the root disk, EFI + cloud-init state,
and a read-**write** host↔guest share folder. Because `br eject` always
powers the guest off cleanly via ACPI, a cartridge is **always** in a consistent
cold-boot state — so you can AirDrop the file to any Mac running bladerunner and
`br boot <file>` just works (a clean cold boot). The clean-shutdown invariant
is what makes AirDrop safe: no dirty filesystem, no host-bound RAM snapshot.

The honest tradeoff: the **disk** is portable (cold-boot on any Mac), while
same-host **RAM resume** is intentionally out of scope — we shut down cleanly
instead of carrying a machine-bound memory image around.

```bash
runner disk pack incus                 # build ./incus.sparseimage (runnable)
runner disk pack incus --ship          # also build ./incus.dmg (compressed AirDrop artifact)
runner boot ./incus.sparseimage        # mount + cold-boot the cartridge
runner boot ./incus.dmg                # materialize a working copy, then boot it
runner eject                           # clean ACPI shutdown, then detach the cartridge
runner disks                           # also lists attached cartridges (booted/ejected)
```

`br disk pack <name>` resolves a catalog/user disk, creates an APFS sparse
image sized to the disk plus headroom (override with `--size`), attaches it, writes
`disk.json`, materializes the bootable `root.img` (via the same image cache /
`qemu-img` path boot uses), and creates `state/` and `share/`. `--out` overrides
the output path; `--ship` additionally produces a compressed read-only `.dmg`
(the AirDrop artifact). `--arch` selects the root image's architecture.

`br boot <cartridge>` mounts the image privately at
`~/.local/state/bladerunner/mnt/<name>/`, roots the VM inside it
(`root.img`, state under `state/`, the RW share at `share/`), and **owns** the
mount — detaching it on exit. A `.dmg` is first converted to a working
`.sparseimage` so the shipped read-only artifact stays pristine.

The **RW share** is exposed to the guest over VirtioFS (tag `bladerunner-share`)
and mounted at `/mnt/share` by a generated systemd `.mount` unit (with an fstab
fallback). VirtioFS maps host files to the guest's mounting context (root), so
the bootstrap `chown`s `/mnt/share` to the SSH user — drop files in `share/` on
the host and read/write them at `/mnt/share` in the guest, and vice versa.

Cartridge layout (at the mountpoint):

```
disk.json            the Manifest (image source is THIS cartridge: root.img)
root.img             the bootable raw disk (sparse on APFS)
state/efi-vars.bin   EFI variable store
state/cloud-init/    cloud-init seed
share/               RW host↔guest VirtioFS folder
```

Cartridges require macOS (they are backed by `hdiutil` + APFS sparse images);
packing also needs `qemu-img`.

## Notes

- The default base image is the **pre-baked bladerunner guest image** (Debian 13 trixie + Incus + `br-agent`, built by `scripts/build-guest-image.sh` and published via the `build-guest-image` workflow under the `guest-image-latest` release). Fresh installs boot it directly — faster first boot, no first-boot apt. It is fetched fail-closed against its published `.sha256` sidecar: a missing, unreachable, or mismatched sidecar is fatal for the hosted image (bladerunner never boots an unverified image).
- **Warned auto-fallback:** if the pre-baked image can't be used — a missing/renamed release asset for the arch, a download error, or a bad/missing/unreachable checksum sidecar — bladerunner emits a `WARN` and automatically falls back to the pinned Debian 13 (trixie) genericcloud qcow2 + first-boot cloud-init path. The Debian fallback is itself SHA-512 fail-closed against an embedded pin, so the invariant holds: you always boot a **verified** image (verified-hosted or verified-Debian), never an unverified one. The chosen path is logged.
- **Escape hatch:** pass `--debian-image` (or set `BLADERUNNER_FORCE_DEBIAN_IMAGE=1`) to force the Debian genericcloud + cloud-init path explicitly — the "bring your own generic image" opt-out. `--hosted-image` (or `BLADERUNNER_FORCE_HOSTED_IMAGE=1`) forces the pre-baked image (already the default). The two are mutually exclusive, and neither can be combined with `--image-url`/`--image-path`. Override to Ubuntu 24.04 or another distribution with `--image-url` or `BLADERUNNER_BASE_IMAGE_URL`.
- The base image can be raw or qcow2 format. qcow2 images are automatically converted to raw via `qemu-img`.
- First boot on the Debian fallback path can take several minutes while cloud-init installs and configures Incus; the pre-baked default skips that.
- Downloaded base images are checksum-verified: the pre-baked default and any disk-manifest-pinned image are SHA-256 verified fail-closed; a user-supplied `--image-url` falls back to a tolerant sidecar check (a missing sidecar is warned, not fatal, since arbitrary upstream hosts rarely publish one).
- `br status` surfaces the pre-baked image build date from `/etc/bladerunner-image-version` when present.
- GUI output is handled by VZ graphics window; serial console is logged at `console.log`.
- Extended operations (download, VM readiness, Incus readiness) show live progress indicators in terminal.
