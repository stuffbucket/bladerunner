# bladerunner

`bladerunner` is a standalone Incus VM runner for macOS built directly on Apple Virtualization.framework via `github.com/Code-Hex/vz/v3`.

It is designed to provide the core behavior of a `colima --runtime incus` setup without Lima/Colima orchestration overhead:

- Architecture-aware defaults (`arm64` and `amd64`) using Ubuntu 24.04 cloud images.
- Incus daemon bootstrapped inside the guest via cloud-init.
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
br start
```

With GUI console window:

```bash
br start --gui
```

Bridged network on `en0`:

```bash
br start --network-mode bridged --bridge-interface en0
```

Custom image path (raw disk image):

```bash
br start --image-path /path/to/base.raw
```

Custom log file path:

```bash
br start --log-path /tmp/bladerunner.log
```

Optional log level:

```bash
BLADERUNNER_LOG_LEVEL=debug br start
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

## Notes

- The base image can be raw or qcow2 format. qcow2 images are automatically converted to raw via `qemu-img`.
- First boot can take several minutes while cloud-init installs and configures Incus.
- GUI output is handled by VZ graphics window; serial console is logged at `console.log`.
- Extended operations (download, VM readiness, Incus readiness) show live progress indicators in terminal.
