package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// DefaultInstanceName is the instance that owns the legacy "Host bladerunner"
// alias and the aggregator file itself. It mirrors config.DefaultInstanceName;
// package ssh keeps its own copy so it stays dependency-free.
const DefaultInstanceName = "bladerunner"

// hostAliasPrefix is the ssh_config Host alias every bladerunner instance is
// reachable under: the default instance as-is, named instances suffixed.
const hostAliasPrefix = "bladerunner"

// instanceDirName is the directory of per-instance ssh config fragments the
// aggregator includes.
const instanceDirName = "config.d"

// configFileName is the aggregator file inside Dir().
const configFileName = "config"

// configLockFileName is the flock claim serializing every read-modify-write of
// the aggregator. It sits in Dir(), NOT in config.d, so the "config.d/*" Include
// glob can never pick it up; the leading dot is a second guard, since glob(3)
// does not match a leading dot with "*".
const configLockFileName = ".config.lock"

// aggregatorLockPath returns the claim guarding the aggregator file.
func aggregatorLockPath() string {
	return filepath.Join(Dir(), configLockFileName)
}

// dirPerm/filePerm are the permissions for the whole generated ssh tree: the
// directory, the config files, the private key and the lock files. ssh refuses
// to use a config file or a key that is group/world readable or writable, so one
// pair of values covers every private file this package writes. The public key
// is the single exception; see pubKeyPerm.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// hostBlockTemplate is the body shared by every generated Host block.
const hostBlockTemplate = `Host {{.Alias}}
    HostName 127.0.0.1
    Port {{.Port}}
    User {{.User}}
    IdentityFile {{.IdentityFile}}
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    LogLevel ERROR
`

// aggregatorTemplate is ~/.config/bladerunner/ssh/config: the legacy default
// instance block, then an Include of the per-instance fragments.
//
// ORDERING IS LOAD-BEARING, and ssh_config has two rules that pull against each
// other here (both verified against OpenSSH with "ssh -F <cfg> -G <host>"):
//
//  1. ssh takes the FIRST value it obtains for a keyword. So whatever is read
//     earlier wins, and the default instance's block must be read before any
//     per-instance fragment that also declares the bare "bladerunner" alias —
//     otherwise a second instance would silently take over the default alias.
//  2. An Include placed after a Host block is treated as PART of that block, so
//     it is only processed when that block matches. A bare "Include" at the
//     bottom of this file would therefore load the per-instance configs only
//     while connecting to "bladerunner" — "ssh bladerunner-demo" would fall
//     through to defaults and quietly dial port 22.
//
// "Match all" reconciles the two: it closes the default instance's block and
// opens an unconditional context, so the Include is processed for every target
// while the default block, being earlier, still wins for the bare alias.
//
// The Include path is absolute on purpose: a relative Include is resolved
// against ~/.ssh, not against the including file's directory. It is a glob
// because Include tolerates a glob that matches nothing, whereas a literal
// missing path is an error.
const aggregatorTemplate = `# Bladerunner SSH configuration
# Generated automatically - do not edit manually
#
# Usage:
#   ssh -F {{.ConfigPath}} {{.Alias}}
#
# Or add to ~/.ssh/config:
#   Include {{.ConfigPath}}

` + hostBlockTemplate + `
# Per-instance configs. This comes AFTER the block above because ssh_config uses
# the first value it obtains for a keyword, so the default instance always wins
# for the bare "{{.Alias}}" alias. "Match all" is required: an Include following
# a Host block belongs to that block and would otherwise only be processed when
# that block matches.
Match all
{{.IncludeLine}}
`

// instanceTemplate is config.d/<name>: the instance under its own alias, plus
// the same settings under the bare alias.
//
// The second block exists because the CLI (`br shell`, `br ssh`, `br incus`)
// connects with "ssh -F <that instance's config path> bladerunner" — a fixed
// alias. When this file is used directly with -F, the bare block is what those
// commands match. When it is reached through the aggregator's Include instead,
// the aggregator's own "Host bladerunner" block was read first and wins, so the
// default instance is never shadowed.
const instanceTemplate = `# Bladerunner SSH configuration for instance "{{.Instance}}"
# Generated automatically - do not edit manually
#
# Usage:
#   ssh -F {{.AggregatorPath}} {{.Alias}}
#   ssh -F {{.ConfigPath}} ` + hostAliasPrefix + `

` + hostBlockTemplate + `
# Same host under the bare alias, for "ssh -F <this file> ` + hostAliasPrefix + `".
Host ` + hostAliasPrefix + `
    HostName 127.0.0.1
    Port {{.Port}}
    User {{.User}}
    IdentityFile {{.IdentityFile}}
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    LogLevel ERROR
`

// aggregatorStubTemplate is written when a named instance starts before the
// default instance has ever generated the aggregator: it wires up the Include
// without inventing a "Host bladerunner" block for an instance that does not
// exist.
const aggregatorStubTemplate = `# Bladerunner SSH configuration
# Generated automatically - do not edit manually

Match all
{{.IncludeLine}}
`

// ConfigParams holds parameters for generating SSH config.
type ConfigParams struct {
	// Instance is the instance name ("" or DefaultInstanceName for the default).
	Instance string
	// Alias is the ssh_config Host alias this file's primary block declares.
	Alias        string
	Port         int
	User         string
	IdentityFile string
	// ConfigPath is the file being written.
	ConfigPath string
	// AggregatorPath is the top-level config that Includes the per-instance dir.
	AggregatorPath string
	// IncludeLine is the absolute-globbed Include directive for config.d.
	IncludeLine string
}

// HostAlias returns the ssh_config Host alias for an instance: "bladerunner"
// for the default instance, "bladerunner-<name>" for any other.
func HostAlias(instance string) string {
	if instance == "" || instance == DefaultInstanceName {
		return hostAliasPrefix
	}
	return hostAliasPrefix + "-" + instance
}

// InstanceConfigDir returns the directory holding per-instance ssh config
// fragments.
func InstanceConfigDir() string {
	return filepath.Join(Dir(), instanceDirName)
}

// InstanceConfigPath returns the config fragment path for a named instance.
func InstanceConfigPath(instance string) string {
	return filepath.Join(InstanceConfigDir(), instance)
}

// includeLine returns the Include directive the aggregator uses to pull in
// every per-instance fragment.
func includeLine() string {
	return "Include " + filepath.Join(InstanceConfigDir(), "*")
}

// safeInstanceName is the allowlist a name must match to be rendered into the
// ssh config tree: an alphanumeric, then alphanumerics, dot, dash or
// underscore. It rejects "." and ".." and any leading dot by construction (the
// first character must be alphanumeric), so a name can never escape config.d,
// and it rejects every control character, ssh_config metacharacter and shell
// metacharacter by construction too.
//
// It is deliberately WIDER than instance.ValidName, which additionally demands
// lowercase and bounds the length at instance.MaxNameLen. A slot basename may
// legitimately carry uppercase, an underscore or a dot and may be longer than
// that bound — see buildStartSpec in cmd/bladerunner/start.go, which documents
// why it does not impose instance.ValidName on those boots.
var safeInstanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validInstanceName rejects names that would escape the config.d directory,
// confuse ssh_config's parser, or turn the command string CommandFor prints
// into something else when pasted into a shell.
//
// This is NOT a redundant second layer. instance.ValidName does not run on
// every route that reaches WriteConfigFor: on the `br start --state-dir <path>`
// route, vmhost.Spec.Name is left empty on purpose, so Spec.validateIdentity
// skips ValidName, and Host.instanceName falls through to
// config.Config.InstanceName — which is filepath.Base of the state directory,
// validated nowhere. internal/vm.Runner.makeReport reads the same unvalidated
// value straight from cfg.InstanceName(). This guard is therefore the only
// check standing between that basename and a file written under ~/.config, so
// it is an allowlist rather than a list of characters someone thought of.
func validInstanceName(instance string) error {
	if instance == "" {
		return errors.New("ssh: instance name must not be empty")
	}
	if !safeInstanceName.MatchString(instance) {
		return fmt.Errorf("ssh: invalid instance name %q: must match %s", instance, safeInstanceName.String())
	}
	return nil
}

// WriteSSHConfig writes the aggregator SSH config for the DEFAULT instance:
// the legacy "Host bladerunner" block plus an Include of the per-instance
// fragments. Returns the path to the generated config file.
//
// Named instances must use WriteInstanceSSHConfig — writing this file is what
// two instances used to clobber, since it is rewritten whole and holds exactly
// one Host block.
//
// The aggregator claim is taken even though this function only ever writes the
// whole file: a named instance is appending an Include to the SAME file under
// the same claim, and a rewrite that races that append destroys it.
func WriteSSHConfig(port int, user string, identityFile string) (string, error) {
	configPath := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), dirPerm); err != nil {
		return "", fmt.Errorf("create ssh config directory: %w", err)
	}
	lock, err := acquireLock(aggregatorLockPath())
	if err != nil {
		return "", err
	}
	defer lock.release()

	params := ConfigParams{
		Instance:       DefaultInstanceName,
		Alias:          hostAliasPrefix,
		Port:           port,
		User:           user,
		IdentityFile:   identityFile,
		ConfigPath:     configPath,
		AggregatorPath: configPath,
		IncludeLine:    includeLine(),
	}
	if err := renderConfig(configPath, aggregatorTemplate, params); err != nil {
		return "", err
	}
	return configPath, nil
}

// WriteInstanceSSHConfig writes the ssh config fragment for a named instance to
// config.d/<name> and makes sure the aggregator includes that directory.
//
// Each instance owns exactly one file, so a whole-file rewrite can no longer
// clobber another instance's settings the way a single shared config did.
// Returns the path to the fragment.
//
// The fragment is published by rename, which means util.WriteFileAtomic briefly
// creates "config.d/<name>.tmp-<random>" beside it — and "Include config.d/*"
// matches that name. This is deliberate and safe, and
// TestLeftoverStagingFileDoesNotBreakInclude holds the claim against the real
// ssh client rather than asserting it here: WriteFileAtomic issues the whole
// fragment in ONE write, so a temp file is either zero bytes (a valid empty
// ssh_config) or a complete copy of the fragment (a duplicate Host block, which
// ssh ignores because it takes the first value it obtains for a keyword). Neither
// is a parse error, so even debris left by a killed process is inert.
//
// The alternative — writing the fragment in place — was strictly worse: it left
// "config.d/<name>" ITSELF truncated inside the same window, and a killed
// process left the instance's real config half written.
//
// The aggregator claim is NOT held across the fragment render. The fragment is a
// file no other instance touches, and taking the claim here would nest with the
// one ensureAggregatorInclude takes, which flock resolves by deadlocking.
func WriteInstanceSSHConfig(instance string, port int, user string, identityFile string) (string, error) {
	if err := validInstanceName(instance); err != nil {
		return "", err
	}
	if err := os.MkdirAll(InstanceConfigDir(), dirPerm); err != nil {
		return "", fmt.Errorf("create ssh config.d directory: %w", err)
	}

	configPath := InstanceConfigPath(instance)
	params := ConfigParams{
		Instance:       instance,
		Alias:          HostAlias(instance),
		Port:           port,
		User:           user,
		IdentityFile:   identityFile,
		ConfigPath:     configPath,
		AggregatorPath: ConfigPath(),
		IncludeLine:    includeLine(),
	}
	if err := renderConfig(configPath, instanceTemplate, params); err != nil {
		return "", err
	}
	if err := ensureAggregatorInclude(params); err != nil {
		return "", err
	}
	return configPath, nil
}

// WriteConfigFor writes the ssh config for an instance, choosing the aggregator
// for the default instance and a config.d fragment for any other. This is the
// entry point callers that only know cfg.InstanceName() should use.
func WriteConfigFor(instance string, port int, user string, identityFile string) (string, error) {
	if instance == "" || instance == DefaultInstanceName {
		return WriteSSHConfig(port, user, identityFile)
	}
	return WriteInstanceSSHConfig(instance, port, user, identityFile)
}

// ensureAggregatorInclude guarantees the aggregator pulls in config.d. If it
// does not exist yet (a named instance started before the default one ever
// did), a stub containing only the Include is written; if it exists but predates
// per-instance configs, the Include is appended — appended, not rewritten, so
// the existing default-instance block stays first and keeps winning.
//
// Read, decide and write are one critical section under the aggregator claim.
// Unlocked, several named instances upgrading at once each read a file with no
// Include and each append one, and a default-instance rewrite landing in the
// middle of an append leaves a file the ssh client refuses outright.
//
// The claim MUST NOT be held by the caller: flock is per open file description,
// so taking it twice from one goroutine deadlocks. WriteInstanceSSHConfig
// therefore renders its own fragment (a file no other instance touches) before
// calling this, not inside it.
func ensureAggregatorInclude(params ConfigParams) error {
	lock, err := acquireLock(aggregatorLockPath())
	if err != nil {
		return err
	}
	defer lock.release()
	return publishAggregatorInclude(params)
}

// publishAggregatorInclude is the body of ensureAggregatorInclude. The caller
// must hold the aggregator claim.
func publishAggregatorInclude(params ConfigParams) error {
	path := params.AggregatorPath
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return renderConfig(path, aggregatorStubTemplate, params)
	case err != nil:
		return fmt.Errorf("read ssh config: %w", err)
	}
	if strings.Contains(string(existing), params.IncludeLine) {
		return nil
	}
	// The append is built in memory and published in one rename, so the file a
	// reader sees is either the old aggregator or the extended one, never a
	// concatenation caught halfway.
	var buf bytes.Buffer
	buf.Write(existing)
	// "Match all" first: the legacy file ends inside its Host block, and an
	// Include there would only be processed for that host (see
	// aggregatorTemplate).
	fmt.Fprintf(&buf, "\nMatch all\n%s\n", params.IncludeLine)
	if err := util.WriteFileAtomic(path, buf.Bytes(), filePerm); err != nil {
		return fmt.Errorf("append ssh config include: %w", err)
	}
	return nil
}

// renderConfig publishes tmplText, executed with params, as the whole contents
// of path.
//
// The template is executed into MEMORY and the result published with one
// rename. Executing straight into the destination (which is what this did, with
// O_CREATE|O_WRONLY|O_TRUNC) leaves a window in which the visible file is empty
// or holds half a Host block: a concurrent `br shell`, or the user's own ssh,
// reads that, and a crash inside the window leaves it behind for good. A render
// that fails now leaves the previous config byte-for-byte intact instead of
// having already truncated it.
func renderConfig(path, tmplText string, params ConfigParams) error {
	tmpl, err := template.New("ssh_config").Parse(tmplText)
	if err != nil {
		return fmt.Errorf("parse ssh config template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("render ssh config: %w", err)
	}
	if err := util.WriteFileAtomic(path, buf.Bytes(), filePerm); err != nil {
		return fmt.Errorf("write ssh config %s: %w", path, err)
	}
	return nil
}

// ConfigPath returns the path to the aggregator SSH config file.
func ConfigPath() string {
	return filepath.Join(Dir(), configFileName)
}

// Command returns the SSH command to connect to the default VM.
func Command(configPath string) string {
	return CommandFor(configPath, DefaultInstanceName)
}

// CommandFor returns the SSH command to connect to a named instance through the
// given config file.
func CommandFor(configPath, instance string) string {
	return fmt.Sprintf("ssh -F %s %s", configPath, HostAlias(instance))
}
