package ssh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
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

// dirPerm/filePerm are the permissions for the generated config tree. ssh
// refuses to use a config file that is group/world writable.
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

// validInstanceName rejects names that would escape the config.d directory or
// confuse ssh_config's Host patterns. internal/instance.ValidName is the
// authoritative check on the way in; this is the local guard so a bad name can
// never turn into a path traversal or a wildcard alias.
func validInstanceName(instance string) error {
	if instance == "" {
		return errors.New("ssh: instance name must not be empty")
	}
	if instance == "." || instance == ".." || strings.HasPrefix(instance, ".") {
		return fmt.Errorf("ssh: invalid instance name %q", instance)
	}
	if strings.ContainsAny(instance, `/\ *?[]!"'`+"\t\n") {
		return fmt.Errorf("ssh: invalid instance name %q", instance)
	}
	return nil
}

// WriteSSHConfig writes the aggregator SSH config for the DEFAULT instance:
// the legacy "Host bladerunner" block plus an Include of the per-instance
// fragments. Returns the path to the generated config file.
//
// Named instances must use WriteInstanceSSHConfig — writing this file is what
// two instances used to clobber, since it is opened O_TRUNC and holds exactly
// one Host block.
func WriteSSHConfig(port int, user string, identityFile string) (string, error) {
	configPath := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), dirPerm); err != nil {
		return "", fmt.Errorf("create ssh config directory: %w", err)
	}

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
// Each instance owns exactly one file, so the O_TRUNC write can no longer
// clobber another instance's settings the way a single shared config did.
// Returns the path to the fragment.
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
func ensureAggregatorInclude(params ConfigParams) error {
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("open ssh config: %w", err)
	}
	defer func() { _ = f.Close() }()
	// "Match all" first: the legacy file ends inside its Host block, and an
	// Include there would only be processed for that host (see
	// aggregatorTemplate).
	if _, err := fmt.Fprintf(f, "\nMatch all\n%s\n", params.IncludeLine); err != nil {
		return fmt.Errorf("append ssh config include: %w", err)
	}
	return nil
}

// renderConfig executes tmplText into path, truncating any previous content.
func renderConfig(path, tmplText string, params ConfigParams) error {
	tmpl, err := template.New("ssh_config").Parse(tmplText)
	if err != nil {
		return fmt.Errorf("parse ssh config template: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("create ssh config file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := tmpl.Execute(f, params); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
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
