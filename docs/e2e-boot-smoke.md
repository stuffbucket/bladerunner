# End-to-end boot smoke test

`test/e2e/boot_smoke_test.go` is the opt-in, macOS-only test that proves the
make-or-break path actually works on real hardware:

    VZ boot → pre-baked image (or Debian cloud-init on the escape hatch) → Incus reachable over vsock

CI-Linux cannot run Virtualization.framework, and the only other integration
test is the opt-in cartridge `hdiutil` one, so without this test a regression in
"does a VM actually come up and run Incus" is invisible to CI. This is that
missing boot-verify gate (#154), and it is the mechanism the "default to the
pre-baked guest image" change (#155) relies on.

The **default** mode boots the pre-baked hosted guest image (the shipped
default). `BLADERUNNER_E2E_DEBIAN=1` forces the Debian escape hatch
(`--debian-image`) instead, which boot-verifies the warned-fallback path.

## What it asserts

From a clean, isolated state the test:

1. builds + codesigns `br` (or uses a signed binary you point it at),
2. runs `br start` as a subprocess to bring a VM up,
3. waits (bounded) for `br ls --json` to return valid JSON — Incus answering an
   authenticated call inside the guest is the real readiness signal (a bare `br
   status`=="running" can flip optimistically mid-provision); `br status` is polled
   alongside only for diagnostics,
4. treats that valid-JSON `br ls` as the proof the Incus API answers inside the
   guest,
5. **always** tears the VM down (`br stop --force`) and reaps the `br start`
   process in `t.Cleanup`, even on failure or timeout, so a hung boot can never
   strand a VM or wedge the machine.

State is fully isolated: the test pins `BLADERUNNER_STATE_DIR`, `HOME`, and the
XDG dirs to `t.TempDir()` slots, so it never touches your real
`~/.local/state/bladerunner` and SSH keys / OIDC identities land in the sandbox.

## Why it drives a signed subprocess (not `vm.StartVM` in-process)

A plain `go test` binary is **not** codesigned with the
`com.apple.security.virtualization` entitlement. Starting a VZ VM in-process from
such a binary fails with the unsigned-for-VZ error (see
`internal/vm/signing_error.go`, #134). So the test drives the **signed `br`
binary as a subprocess** instead of calling `vm.StartVM` directly. The test
builds and codesigns `br` via `make sign` (which uses `vz.entitlements`) and
verifies the entitlement is present before booting; if you supply your own binary
via `BLADERUNNER_E2E_BIN`, make sure it is signed (`make sign`, or
`brew install stuffbucket/tap/bladerunner`).

## Running it locally

The test is skipped unless `BLADERUNNER_E2E=1` and `GOOS=darwin`, so a normal
`go test ./...` never boots a VM.

```sh
# Build + codesign br, then run the smoke test (default: pre-baked hosted image):
make sign
BLADERUNNER_E2E=1 go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/

# Or let the test build + sign br itself (it runs `make sign`):
BLADERUNNER_E2E=1 go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/
```

### Environment knobs

| Variable                       | Meaning                                                                        |
| ------------------------------ | ------------------------------------------------------------------------------ |
| `BLADERUNNER_E2E=1`            | **Required.** Opt in to the real boot.                                          |
| `BLADERUNNER_E2E_DEBIAN=1`     | Force the Debian escape hatch (`--debian-image`) instead of the pre-baked default. |
| `BLADERUNNER_E2E_HOSTED=1`     | No-op alias (the hosted image is already the default); retained for compatibility. |
| `BLADERUNNER_E2E_BIN=/path/br` | Use an already-signed `br` instead of building one. You vouch that it's signed. |
| `BLADERUNNER_E2E_BOOT_TIMEOUT` | Readiness budget for first boot (Go duration; default `15m`).                   |

Both provisioning paths are covered so this test can serve as the boot-verify
gate for either the pre-baked default or the Debian/cloud-init escape hatch:

```sh
# Debian escape-hatch / warned-fallback path:
BLADERUNNER_E2E=1 BLADERUNNER_E2E_DEBIAN=1 \
  go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/
```

First boot on the Debian path downloads the guest image and installs Incus via
cloud-init, which can take many minutes on stock hardware — hence the generous
default budget; the pre-baked default skips that. The `go test -timeout` must
comfortably exceed `BLADERUNNER_E2E_BOOT_TIMEOUT` plus teardown.

## Running it in CI

`.github/workflows/e2e-boot.yml` runs the test on a **self-hosted** macOS runner
(`runs-on: [self-hosted, macOS]`), triggered manually via `workflow_dispatch`.
Its `boot_timeout` input maps to `BLADERUNNER_E2E_BOOT_TIMEOUT`; the job boots the
pre-baked default (set `BLADERUNNER_E2E_DEBIAN=1` locally to exercise the escape
hatch).

> **GitHub-hosted macOS runners cannot run nested Virtualization.framework**, so
> a VZ VM will not start there. The job must run on a self-hosted macOS runner
> (the stuffbucket mac mini) or be run locally. The workflow is deliberately
> `workflow_dispatch`-only and **non-blocking** — it is never a required status
> check, so a slow or flaky boot can't block a merge.
