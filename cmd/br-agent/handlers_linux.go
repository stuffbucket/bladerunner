//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/agent"
)

// Paths and well-known names used by the handlers. Centralised so tests can
// override them and so goconst stays happy.
const (
	configDir         = "/etc/bladerunner"
	configFileName    = "config.json"
	bladerunnerUnits  = "bladerunner-*.service"
	incusUserName     = "incus"
	authorizedKeysRel = ".ssh/authorized_keys"
	sshDirName        = ".ssh"

	// configFilePerm is the mode used when writing config.json. Readable
	// by anyone; only root may write it.
	configFilePerm = 0o644
	sshDirPerm     = 0o700
	keyFilePerm    = 0o600

	readyWaitCmdTimeout = 60 * time.Second
)

// runner is the small subset of os/exec the handlers need. Tests inject a
// fake runner to avoid spawning real processes.
type runner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production implementation.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

// handlerState bundles the dependencies the handlers need so they can be
// overridden in tests.
type handlerState struct {
	runner   runner
	rootDir  string // typically "" (real fs); tests set this to a tempdir.
	lookupFn func(name string) (*user.User, error)
}

// defaultState constructs the production handler state.
func defaultState() *handlerState {
	return &handlerState{
		runner:   execRunner{},
		lookupFn: user.Lookup,
	}
}

// state is the package-level handler state. Tests replace it.
var state = defaultState()

func handleConfigPush(ctx context.Context, msg *agent.Message) *agent.Message {
	var args agent.ConfigPushArgs
	if err := agent.DecodeArgs(msg, &args); err != nil {
		return &agent.Message{Error: err.Error()}
	}
	if err := applyConfigPush(ctx, state, &args); err != nil {
		return &agent.Message{Error: err.Error()}
	}
	return &agent.Message{Response: agent.ResponseOK}
}

func applyConfigPush(ctx context.Context, s *handlerState, args *agent.ConfigPushArgs) error {
	if err := writeConfigJSON(s.rootDir, args); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	keys := map[string]string{}
	if args.OIDCIssuer != "" {
		keys["core.oidc.issuer"] = args.OIDCIssuer
	}
	if args.OIDCClientID != "" {
		keys["core.oidc.client.id"] = args.OIDCClientID
	}
	if args.OIDCAudience != "" {
		keys["core.oidc.audience"] = args.OIDCAudience
	}
	if args.CoreHTTPSAddress != "" {
		keys["core.https_address"] = args.CoreHTTPSAddress
	}
	for k, v := range keys {
		if err := s.runner.Run(ctx, "incus", "config", "set", k, v); err != nil {
			log.Printf("br-agent: incus config set %s failed: %v", k, err)
		}
	}

	// Reload bladerunner-* units if any are loaded; ignore failure (the
	// units may not exist yet in pre-baked images).
	_ = s.runner.Run(ctx, "systemctl", "daemon-reload")
	_ = s.runner.Run(ctx, "systemctl", "try-restart", bladerunnerUnits)
	return nil
}

func writeConfigJSON(rootDir string, args *agent.ConfigPushArgs) error {
	dir := filepath.Join(rootDir, configDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, configFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, configFilePerm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func handleReadyWait(ctx context.Context, _ *agent.Message) *agent.Message {
	info, err := waitForIncus(ctx, state)
	if err != nil {
		return &agent.Message{Error: err.Error()}
	}
	raw, err := agent.EncodeArgs(info)
	if err != nil {
		return &agent.Message{Error: err.Error()}
	}
	return &agent.Message{Response: agent.ResponseReady, Args: raw}
}

func waitForIncus(ctx context.Context, s *handlerState) (*agent.ReadyWaitResponse, error) {
	waitCtx, cancel := context.WithTimeout(ctx, readyWaitCmdTimeout)
	defer cancel()
	if err := s.runner.Run(waitCtx, "incus", "admin", "waitready", "--timeout=60"); err != nil {
		// `incus admin init --auto` is safe to run repeatedly; it
		// no-ops if storage and network are already initialised.
		if initErr := s.runner.Run(ctx, "incus", "admin", "init", "--auto"); initErr != nil {
			return nil, fmt.Errorf("incus init: %w", initErr)
		}
		if err := s.runner.Run(ctx, "incus", "admin", "waitready", "--timeout=60"); err != nil {
			return nil, fmt.Errorf("incus waitready: %w", err)
		}
	}

	resp := &agent.ReadyWaitResponse{}
	if out, err := s.runner.Output(ctx, "incus", "version"); err == nil {
		resp.IncusVersion = strings.TrimSpace(string(out))
	}
	if out, err := s.runner.Output(ctx, "incus", "config", "get", "core.https_address"); err == nil {
		resp.APIAddress = strings.TrimSpace(string(out))
	}
	return resp, nil
}

func handleUserSync(ctx context.Context, msg *agent.Message) *agent.Message {
	var args agent.UserSyncArgs
	if err := agent.DecodeArgs(msg, &args); err != nil {
		return &agent.Message{Error: err.Error()}
	}
	if err := applyUserSync(ctx, state, args.AuthorizedKeys); err != nil {
		return &agent.Message{Error: err.Error()}
	}
	return &agent.Message{Response: agent.ResponseOK}
}

func applyUserSync(_ context.Context, s *handlerState, keys string) error {
	homeDir, uid, gid, err := resolveIncusUser(s)
	if err != nil {
		return err
	}
	sshDir := filepath.Join(homeDir, sshDirName)
	if err := os.MkdirAll(sshDir, sshDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", sshDir, err)
	}
	if uid >= 0 {
		_ = os.Chown(sshDir, uid, gid)
	}
	target := filepath.Join(homeDir, authorizedKeysRel)
	tmp := target + ".tmp"
	content := keys
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(tmp, []byte(content), keyFilePerm); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if uid >= 0 {
		_ = os.Chown(tmp, uid, gid)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("rename %s: %w", target, err)
	}
	return nil
}

// resolveIncusUser returns the home dir and numeric uid/gid for the incus
// user. If lookup fails (e.g. in tests with rootDir set), it falls back to
// rootDir/home/incus with -1/-1 so Chown is skipped.
func resolveIncusUser(s *handlerState) (string, int, int, error) {
	if s.rootDir != "" {
		home := filepath.Join(s.rootDir, "home", incusUserName)
		if err := os.MkdirAll(home, 0o755); err != nil {
			return "", 0, 0, err
		}
		return home, -1, -1, nil
	}
	u, err := s.lookupFn(incusUserName)
	if err != nil {
		return "", 0, 0, fmt.Errorf("lookup user %s: %w", incusUserName, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse gid: %w", err)
	}
	return u.HomeDir, uid, gid, nil
}
