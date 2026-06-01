//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/agent"
)

const (
	testIssuer       = "http://issuer"
	testIncusAPIAddr = "[::]:8443"
)

// fakeRunner records every command it was asked to run and returns canned
// stdout/error for each. It is safe for concurrent use.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	stdout  map[string]string
	errs    map[string]error
	runFunc func(name string, args []string) error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		stdout: map[string]string{},
		errs:   map[string]error{},
	}
}

func (f *fakeRunner) key(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	err := f.errs[f.key(name, args)]
	rf := f.runFunc
	f.mu.Unlock()
	if rf != nil {
		return rf(name, args)
	}
	return err
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	out := f.stdout[f.key(name, args)]
	err := f.errs[f.key(name, args)]
	f.mu.Unlock()
	return []byte(out), err
}

func (f *fakeRunner) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func TestApplyConfigPushWritesConfigAndSetsIncusKeys(t *testing.T) {
	tmp := t.TempDir()
	fr := newFakeRunner()
	s := &handlerState{runner: fr, rootDir: tmp}
	args := &agent.ConfigPushArgs{
		OIDCIssuer:       testIssuer,
		OIDCClientID:     "bladerunner",
		OIDCAudience:     "bladerunner",
		CoreHTTPSAddress: testIncusAPIAddr,
	}
	if err := applyConfigPush(context.Background(), s, args); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfgPath := filepath.Join(tmp, configDir, configFileName)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var roundTrip agent.ConfigPushArgs
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTrip.OIDCIssuer != testIssuer {
		t.Fatalf("OIDCIssuer = %q", roundTrip.OIDCIssuer)
	}

	wantCalls := []string{
		"incus config set oidc.issuer " + testIssuer,
		"incus config set oidc.client.id bladerunner",
		"incus config set oidc.audience bladerunner",
		"incus config set core.https_address " + testIncusAPIAddr,
		"systemctl daemon-reload",
	}
	for _, w := range wantCalls {
		if !fr.ran(w) {
			t.Errorf("missing call: %q", w)
		}
	}
}

func TestApplyConfigPushIncusFailureIsLoggedNotFatal(t *testing.T) {
	tmp := t.TempDir()
	fr := newFakeRunner()
	fr.errs["incus config set oidc.issuer "+testIssuer] = errors.New("boom")
	s := &handlerState{runner: fr, rootDir: tmp}
	args := &agent.ConfigPushArgs{OIDCIssuer: testIssuer}
	if err := applyConfigPush(context.Background(), s, args); err != nil {
		t.Fatalf("apply returned error despite tolerating incus failures: %v", err)
	}
}

func TestWaitForIncusInitsOnFailedWait(t *testing.T) {
	fr := newFakeRunner()
	// First waitready fails -> handler calls `incus admin init --auto`,
	// then retries waitready. Configure the runner so the retry succeeds.
	var waitCount int
	var mu sync.Mutex
	fr.runFunc = func(name string, args []string) error {
		if name == "incus" && len(args) == 3 && args[0] == "admin" && args[1] == "waitready" {
			mu.Lock()
			waitCount++
			c := waitCount
			mu.Unlock()
			if c == 1 {
				return errors.New("not ready")
			}
			return nil
		}
		return nil
	}
	fr.stdout["incus version"] = "Client version: 6.0\nServer version: 6.0"
	fr.stdout["incus config get core.https_address"] = testIncusAPIAddr

	s := &handlerState{runner: fr}
	resp, err := waitForIncus(context.Background(), s)
	if err != nil {
		t.Fatalf("waitForIncus: %v", err)
	}
	if !fr.ran("incus admin init --auto") {
		t.Errorf("expected `incus admin init --auto` after failed wait")
	}
	if !strings.Contains(resp.IncusVersion, "6.0") {
		t.Errorf("incus version = %q", resp.IncusVersion)
	}
	if resp.APIAddress != testIncusAPIAddr {
		t.Errorf("api address = %q", resp.APIAddress)
	}
}

func TestApplyUserSyncWritesAuthorizedKeysAtomically(t *testing.T) {
	tmp := t.TempDir()
	s := &handlerState{runner: newFakeRunner(), rootDir: tmp}
	keys := "ssh-ed25519 AAAA host\nssh-ed25519 BBBB other"
	if err := applyUserSync(context.Background(), s, keys); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "home", incusUserName, authorizedKeysRel))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("expected trailing newline, got %q", string(got))
	}
	if !strings.Contains(string(got), "ssh-ed25519 AAAA host") {
		t.Errorf("missing first key: %q", string(got))
	}
	if !strings.Contains(string(got), "ssh-ed25519 BBBB other") {
		t.Errorf("missing second key: %q", string(got))
	}

	info, err := os.Stat(filepath.Join(tmp, "home", incusUserName, authorizedKeysRel))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != keyFilePerm {
		t.Errorf("mode = %o, want %o", info.Mode().Perm(), keyFilePerm)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	resp := dispatch(context.Background(), &agent.Message{Command: "agent.bogus"})
	if resp.Error == "" {
		t.Fatalf("expected error response")
	}
}
