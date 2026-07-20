// Package e2e holds the opt-in, host-only end-to-end smoke test that boots a
// real Bladerunner VM on a Mac and proves Incus answers inside the guest.
//
// It is gated behind BLADERUNNER_E2E=1 and runtime.GOOS == "darwin" so a plain
// `go test ./...` never boots a VM (the default suite stays fast and portable,
// and CI-Linux — which cannot run Virtualization.framework — is unaffected).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Environment knobs (all optional; the default gives a plain cloud-init boot):
//
//	BLADERUNNER_E2E=1            required — opt in to the real boot
//	BLADERUNNER_E2E_BIN=/path    use an already signed `br` instead of building one
//	BLADERUNNER_E2E_BOOT_TIMEOUT first-boot readiness budget (Go duration, default 15m)
//
// Provisioning is unified onto the single cloud-init path (#160). Phase 3 (#155)
// will reintroduce hosted-image selection and its own e2e coverage.
const (
	envEnable      = "BLADERUNNER_E2E"
	envBin         = "BLADERUNNER_E2E_BIN"
	envBootTimeout = "BLADERUNNER_E2E_BOOT_TIMEOUT"

	// defaultBootTimeout bounds the wait for Incus to answer from a clean state.
	// First boot downloads the guest image and installs Incus via cloud-init,
	// which can exceed 10m on stock M-series hardware; 15m absorbs that.
	defaultBootTimeout = 15 * time.Minute

	// pollInterval is how often we re-check `br status --json` for readiness.
	pollInterval = 10 * time.Second

	// stopTimeout bounds teardown so a wedged guest cannot strand the test.
	stopTimeout = 2 * time.Minute

	// cmdTimeout bounds a single short control-plane subprocess (status/ls/stop).
	cmdTimeout = 90 * time.Second
)

// TestE2EBootSmoke brings a VM up from a clean, isolated state, waits (bounded)
// for Incus to answer, runs one trivial guest op to prove the guest works, and
// always tears the VM down — even on failure or timeout.
//
// It drives the SIGNED `br` binary as a subprocess rather than calling
// vm.StartVM in-process: a plain `go test` binary is not codesigned with the
// com.apple.security.virtualization entitlement, so an in-process VZ start would
// fail with the unsigned-for-VZ error (see internal/vm/signing_error.go, #134).
func TestE2EBootSmoke(t *testing.T) {
	if os.Getenv(envEnable) != "1" {
		t.Skipf("set %s=1 to run the end-to-end boot smoke test (boots a real VM)", envEnable)
	}
	if runtime.GOOS != "darwin" {
		t.Skipf("e2e boot smoke test requires macOS Virtualization.framework; GOOS=%s", runtime.GOOS)
	}

	// Provisioning is unified onto the single cloud-init path (#160); there is no
	// merged CLI flag to force the hosted image yet. Phase 3 (#155) will
	// reintroduce hosted-image selection along with its e2e coverage.
	t.Logf("e2e boot smoke: provisioning path = cloud-init")

	bin := signedBinary(t)

	// Fully isolate host/VM state under a temp dir so this run cannot touch (or
	// be confused by) a real ~/.local/state/bladerunner. BLADERUNNER_STATE_DIR
	// is the single override every br command honors (config.DefaultStateDir);
	// HOME/XDG are pinned too so SSH keys and identities land in the sandbox.
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	env := isolatedEnv(stateDir, homeDir)

	boot := bootTimeout(t)

	// Teardown is registered BEFORE we start the VM so a failure anywhere below
	// (including a panic or a t.Fatal in the readiness wait) still ejects the
	// guest and reaps the `br start` process. It runs even if start never became
	// ready — br stop is a no-op when nothing is running.
	startCtx, cancelStart := context.WithCancel(context.Background())
	var startCmd *exec.Cmd
	t.Cleanup(func() {
		teardown(t, bin, env, cancelStart, startCmd)
	})

	// Launch `br start --json` as a long-lived foreground server subprocess. It
	// blocks (serving the control socket) until stopped; readiness is observed
	// out-of-band by polling `br status --json`, not by waiting on this process.
	startArgs := []string{"start", "--json", "--timeout", boot.String()}
	startCmd = exec.CommandContext(startCtx, bin, startArgs...)
	startCmd.Env = env
	var startOut startLog
	startCmd.Stdout = &startOut
	startCmd.Stderr = &startOut
	if err := startCmd.Start(); err != nil {
		t.Fatalf("launch `br %s`: %v", strings.Join(startArgs, " "), err)
	}
	t.Logf("launched `br %s` (pid %d)", strings.Join(startArgs, " "), startCmd.Process.Pid)

	// Wait for Incus to actually answer an authenticated `br ls`. That — not a
	// bare `br status`=="running" (which reflects guest liveness, can flip
	// optimistically mid-provision, and says nothing about client-cert
	// authorization) — is the real readiness signal. Poll until it returns valid
	// JSON or the boot budget elapses.
	out, err := waitForIncus(t, bin, env, boot, startCmd, &startOut)
	if err != nil {
		t.Fatalf("Incus not reachable within %s: %v\n--- br start output ---\n%s",
			boot, err, startOut.String())
	}
	t.Logf("guest op OK: `br ls --json` returned valid JSON:\n%s", strings.TrimSpace(out))
}

// signedBinary returns a path to a `br` binary codesigned with the
// Virtualization entitlement. It honors BLADERUNNER_E2E_BIN when set (the
// caller vouches it is signed), otherwise runs `make sign` at the repo root,
// which builds then codesigns with vz.entitlements.
func signedBinary(t *testing.T) string {
	t.Helper()

	if bin := os.Getenv(envBin); bin != "" {
		// The operator explicitly points BLADERUNNER_E2E_BIN at their own signed
		// binary in this manually-gated, host-only test; it is a trusted input.
		if _, err := os.Stat(bin); err != nil { //nolint:gosec // operator-supplied path in an opt-in local test
			t.Fatalf("%s=%q does not exist: %v", envBin, bin, err)
		}
		t.Logf("using pre-signed binary from %s=%s", envBin, bin)
		return bin
	}

	root := repoRoot(t)
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("make not found in PATH (needed to build+sign br): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, makeBin, "-C", root, "sign")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("`make -C %s sign` failed: %v\n%s", root, err, out)
	}

	bin := filepath.Join(root, "bin", "br")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("signed binary missing at %s after `make sign`: %v", bin, err)
	}
	assertVirtualizationEntitlement(t, bin)
	return bin
}

// assertVirtualizationEntitlement fails fast if bin is not codesigned with the
// com.apple.security.virtualization entitlement — the whole point of driving a
// signed subprocess. Without it, `br start` would fail with the unsigned-for-VZ
// error and the readiness wait would just time out with a confusing message.
func assertVirtualizationEntitlement(t *testing.T, bin string) {
	t.Helper()
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		t.Logf("codesign not found; skipping entitlement verification of %s", bin)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, codesign, "-d", "--entitlements", "-", "--xml", bin).CombinedOutput()
	if err != nil {
		t.Logf("could not read entitlements of %s (continuing): %v\n%s", bin, err, out)
		return
	}
	if !strings.Contains(string(out), "com.apple.security.virtualization") {
		t.Fatalf("%s is not codesigned with com.apple.security.virtualization; "+
			"run `make sign` (VZ will reject the VM otherwise). entitlements:\n%s", bin, out)
	}
}

// repoRoot returns the module root (where the Makefile lives) by walking up from
// this test file's directory until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (go.mod) above %s", dir)
		}
		dir = parent
	}
}

// isolatedEnv returns the parent process environment with the state/home
// overrides that sandbox a br run: BLADERUNNER_STATE_DIR (the master state
// override), plus HOME/XDG_STATE_HOME/XDG_CONFIG_HOME so SSH keys and OIDC
// identities are written under the temp dirs, not the developer's real home.
func isolatedEnv(stateDir, homeDir string) []string {
	overrides := map[string]string{
		"BLADERUNNER_STATE_DIR": stateDir,
		"HOME":                  homeDir,
		"XDG_STATE_HOME":        filepath.Join(homeDir, ".local", "state"),
		"XDG_CONFIG_HOME":       filepath.Join(homeDir, ".config"),
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// bootTimeout resolves the readiness budget from BLADERUNNER_E2E_BOOT_TIMEOUT,
// falling back to defaultBootTimeout.
func bootTimeout(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv(envBootTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("invalid %s=%q: %v", envBootTimeout, v, err)
		}
		return d
	}
	return defaultBootTimeout
}

// waitForRunning polls `br status --json` until it reports status "running"
// (guest up + Incus reachable), the deadline passes, or the `br start` process
// dies early. Returns nil once running.
func waitForIncus(t *testing.T, bin string, env []string, within time.Duration, startCmd *exec.Cmd, startOut *startLog) (string, error) {
	t.Helper()
	deadline := time.Now().Add(within)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Watch for the start process exiting early (a signing/config error would
	// make it exit before Incus ever comes up) so we fail fast instead of
	// polling for the full timeout.
	exited := make(chan error, 1)
	go func() { exited <- startCmd.Wait() }()

	var lastErr error
	for {
		out, err := runCmd(t, bin, env, cmdTimeout, "ls", "--json")
		if err == nil && json.Valid(bytes.TrimSpace([]byte(out))) {
			return out, nil
		}
		lastErr = err
		// A connection-refused / not-authorized / not-yet-up `br ls` just means
		// keep waiting; log the status alongside for diagnosis.
		if status, serr := queryStatus(t, bin, env); serr == nil {
			t.Logf("status: %s; `br ls` not ready yet (will retry)", status)
		} else {
			t.Logf("`br ls` not ready yet (will retry): %v", err)
		}

		select {
		case werr := <-exited:
			return "", &earlyExitError{err: werr, log: startOut.String()}
		case <-ticker.C:
			if time.Now().After(deadline) {
				if lastErr != nil {
					return "", fmt.Errorf("deadline exceeded; last `br ls` error: %w", lastErr)
				}
				return "", context.DeadlineExceeded
			}
		}
	}
}

// queryStatus runs `br status --json` and extracts the top-level "status"
// field. A non-running VM reports "stopped"; a booting one may report
// "unreachable" until the guest answers.
func queryStatus(t *testing.T, bin string, env []string) (string, error) {
	t.Helper()
	out, err := runCmd(t, bin, env, cmdTimeout, "status", "--json")
	if err != nil {
		return "", err
	}
	var report struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal([]byte(out), &report); uerr != nil {
		return "", &statusParseError{raw: out, err: uerr}
	}
	return report.Status, nil
}

// runCmd runs a short br subcommand to completion with a bounded timeout,
// returning combined stdout+stderr. Stderr is folded in so diagnostics survive
// in the returned string for t.Log / failure messages.
func runCmd(t *testing.T, bin string, env []string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// teardown always ejects the guest and reaps the `br start` process, even on
// failure or timeout, so a hung boot never strands a VM. It is registered via
// t.Cleanup before the VM is started.
func teardown(t *testing.T, bin string, env []string, cancelStart context.CancelFunc, startCmd *exec.Cmd) {
	t.Helper()
	t.Log("teardown: stopping VM and cleaning up")

	// Force-stop escalates to terminating the host process if graceful ACPI
	// shutdown stalls (e.g. a panicked guest) — exactly the hung-boot case.
	if out, err := runCmd(t, bin, env, stopTimeout, "stop", "--force"); err != nil {
		t.Logf("teardown: `br stop --force` returned %v (may be benign if never started):\n%s", err, out)
	} else {
		t.Logf("teardown: `br stop --force` OK:\n%s", strings.TrimSpace(out))
	}

	// Cancel the start context (kills the foreground server process if stop did
	// not already unblock it) and reap it so no orphan lingers.
	if startCmd != nil && startCmd.Process != nil {
		cancelStart()
		done := make(chan struct{})
		go func() { _, _ = startCmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = startCmd.Process.Kill()
		}
	}
}

// startLog is a concurrency-safe accumulator for the `br start` subprocess
// output, safe to share between the stdout/stderr writers the process feeds and
// the test goroutine that reads it for diagnostics.
type startLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *startLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *startLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// earlyExitError reports that `br start` exited before the VM reached "running".
type earlyExitError struct {
	err error
	log string
}

func (e *earlyExitError) Error() string {
	return "`br start` exited before the VM became ready: " + errString(e.err) +
		"\n--- br start output ---\n" + e.log
}

// statusParseError reports that `br status --json` produced unparseable output.
type statusParseError struct {
	raw string
	err error
}

func (e *statusParseError) Error() string {
	return "parse `br status --json` output: " + errString(e.err) + "\nraw:\n" + e.raw
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
