# End-to-end boot smoke test

`test/e2e/boot_smoke_test.go` is the opt-in, macOS-only test that proves the
make-or-break path actually works on real hardware:

    VZ boot → cloud-init / agent provisioning → Incus reachable over vsock

CI-Linux cannot run Virtualization.framework, and the only other integration
test is the opt-in cartridge `hdiutil` one, so without this test a regression in
"does a VM actually come up and run Incus" is invisible to CI. This is that
missing boot-verify gate (#154), and it is the mechanism the "default to the
pre-baked guest image" change (#155) relies on.

## What it asserts

From a clean, isolated state the test:

1. builds + codesigns `br` (or uses a signed binary you point it at),
2. runs `br start` as a subprocess to bring a VM up,
3. waits (bounded) for `br status --json` to report `running` — the guest is up
   and the control server's guest-liveness probe passes,
4. runs one trivial guest op, `br ls --json`, and requires valid JSON back — this
   proves the Incus API answers inside the guest,
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
# Build + codesign br, then run the smoke test (cloud-init path):
make sign
BLADERUNNER_E2E=1 go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/

# Or let the test build + sign br itself (it runs `make sign`):
BLADERUNNER_E2E=1 go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/
```

### Environment knobs

| Variable                       | Meaning                                                                        |
| ------------------------------ | ------------------------------------------------------------------------------ |
| `BLADERUNNER_E2E=1`            | **Required.** Opt in to the real boot.                                          |
| `BLADERUNNER_E2E_PREBAKED=1`   | Drive the pre-baked/agent path (`--use-guest-agent`) instead of cloud-init.     |
| `BLADERUNNER_E2E_BIN=/path/br` | Use an already-signed `br` instead of building one. You vouch that it's signed. |
| `BLADERUNNER_E2E_BOOT_TIMEOUT` | Readiness budget for first boot (Go duration; default `15m`).                   |

Both provisioning paths are covered so this test can serve as the boot-verify
gate for either the cloud-init default or the pre-baked/agent flip:

```sh
# Pre-baked / agent provisioning path:
BLADERUNNER_E2E=1 BLADERUNNER_E2E_PREBAKED=1 \
  go test -run TestE2EBootSmoke -count=1 -timeout 30m -v ./test/e2e/
```

First boot downloads the guest image and (on the cloud-init path) installs Incus,
which can take many minutes on stock hardware — hence the generous default
budget. The `go test -timeout` must comfortably exceed
`BLADERUNNER_E2E_BOOT_TIMEOUT` plus teardown.

## Running it in CI

`.github/workflows/e2e-boot.yml` runs the test on a **self-hosted** macOS runner
(`runs-on: [self-hosted, macOS]`), triggered manually via `workflow_dispatch`.
Its `prebaked` and `boot_timeout` inputs map to the env knobs above.

> **GitHub-hosted macOS runners cannot run nested Virtualization.framework**, so
> a VZ VM will not start there. The job must run on a self-hosted macOS runner
> (the stuffbucket mac mini) or be run locally. The workflow is deliberately
> `workflow_dispatch`-only and **non-blocking** — it is never a required status
> check, so a slow or flaky boot can't block a merge.
