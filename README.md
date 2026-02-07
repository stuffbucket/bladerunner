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

- macOS 13+ recommended.
- Xcode command line tools.
- Binary must be code-signed with Virtualization entitlement.
- If you want bridged networking, add the VM networking entitlement too.

## Build

```bash
go mod tidy
go build -o bladerunner ./cmd/bladerunner
```

## Entitlements

Create `vz.entitlements`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.virtualization</key>
  <true/>
  <key>com.apple.vm.networking</key>
  <true/>
</dict>
</plist>
```

Sign the binary:

```bash
codesign --entitlements vz.entitlements -s - ./bladerunner
```

## Run

Default (shared network + localhost forwarding + GUI):

```bash
./bladerunner
```

Bridged network on `en0`:

```bash
./bladerunner --network-mode bridged --bridge-interface en0
```

Headless:

```bash
./bladerunner --gui=false
```

Custom image path (raw disk image):

```bash
./bladerunner --image-path /path/to/base.raw
```

Custom log file path:

```bash
./bladerunner --log-path /tmp/bladerunner.log
```

Optional log level:

```bash
BLADERUNNER_LOG_LEVEL=debug ./bladerunner
```

## Access

After startup, the tool prints a report and writes JSON report data to:

- `~/.bladerunner/<name>/startup-report.json`

Key defaults:

- Incus API/UI endpoint: `https://127.0.0.1:18443`
- SSH endpoint: `127.0.0.1:6022`
- Dashboard URL: `https://127.0.0.1:18443/ui/`
- Log file: `~/.bladerunner/<name>/bladerunner.log` (rotated with compression)

Example SSH:

```bash
ssh -p 6022 incus@127.0.0.1
```

Example REST call:

```bash
curl --cert ~/.bladerunner/incus-vm/client.crt --key ~/.bladerunner/incus-vm/client.key -k https://127.0.0.1:18443/1.0
```

## Notes

- The base image must be a **raw** disk image. qcow2 images are rejected.
- First boot can take several minutes while cloud-init installs and configures Incus.
- GUI output is handled by VZ graphics window; serial console is logged at `console.log`.
- Extended operations (download, VM readiness, Incus readiness) show live progress indicators in terminal.
