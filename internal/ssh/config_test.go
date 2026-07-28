package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// readFile reads a generated config file or fails the test.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWriteSSHConfigAggregatorIncludesInstanceDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := WriteSSHConfig(6022, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteSSHConfig() error = %v", err)
	}
	got := readFile(t, path)

	if !strings.Contains(got, "Host bladerunner\n") {
		t.Error("aggregator missing the legacy 'Host bladerunner' block")
	}
	if !strings.Contains(got, "Port 6022") {
		t.Error("aggregator missing 'Port 6022'")
	}
	include := "Include " + filepath.Join(InstanceConfigDir(), "*")
	if !strings.Contains(got, include) {
		t.Errorf("aggregator missing %q\n%s", include, got)
	}
	// ssh_config takes the FIRST value obtained for a keyword, so the default
	// instance's block must precede the Include or a config.d fragment could
	// take over the bare alias.
	if strings.Index(got, "Host bladerunner\n") > strings.Index(got, include) {
		t.Error("Include must come AFTER the default 'Host bladerunner' block")
	}
	if !filepath.IsAbs(strings.TrimPrefix(include, "Include ")) {
		t.Error("Include path must be absolute: a relative Include resolves against ~/.ssh")
	}
}

func TestWriteInstanceSSHConfigDoesNotClobber(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	aggPath, err := WriteSSHConfig(6022, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteSSHConfig() error = %v", err)
	}

	onePath, err := WriteInstanceSSHConfig("demo", 51001, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteInstanceSSHConfig(demo) error = %v", err)
	}
	twoPath, err := WriteInstanceSSHConfig("other", 51002, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteInstanceSSHConfig(other) error = %v", err)
	}

	if onePath == twoPath || onePath == aggPath {
		t.Fatalf("instances share a config file: %q %q %q", aggPath, onePath, twoPath)
	}

	// Neither instance disturbed the default instance's block.
	agg := readFile(t, aggPath)
	if !strings.Contains(agg, "Port 6022") {
		t.Errorf("aggregator lost the default instance port:\n%s", agg)
	}
	if strings.Contains(agg, "51001") || strings.Contains(agg, "51002") {
		t.Errorf("instance ports leaked into the aggregator:\n%s", agg)
	}

	one := readFile(t, onePath)
	if !strings.Contains(one, "Host bladerunner-demo\n") {
		t.Errorf("instance config missing its own alias:\n%s", one)
	}
	if !strings.Contains(one, "Port 51001") {
		t.Errorf("instance config missing its port:\n%s", one)
	}
	// The CLI always connects via the fixed "bladerunner" alias with -F <this
	// file>, so the bare block has to be here too.
	if !strings.Contains(one, "\nHost bladerunner\n") {
		t.Errorf("instance config missing the bare-alias block:\n%s", one)
	}

	two := readFile(t, twoPath)
	if strings.Contains(two, "51001") {
		t.Errorf("second instance clobbered by the first:\n%s", two)
	}
	if !strings.Contains(two, "Port 51002") {
		t.Errorf("second instance missing its port:\n%s", two)
	}

	// Rewriting instance one (a restart on a new port) leaves two alone.
	if _, err := WriteInstanceSSHConfig("demo", 51003, "testuser", "/path/to/key"); err != nil {
		t.Fatalf("rewrite demo: %v", err)
	}
	if got := readFile(t, twoPath); !strings.Contains(got, "Port 51002") {
		t.Errorf("rewriting one instance clobbered another:\n%s", got)
	}
	if got := readFile(t, onePath); !strings.Contains(got, "Port 51003") {
		t.Errorf("rewrite did not take effect:\n%s", got)
	}
}

func TestWriteInstanceSSHConfigCreatesAggregatorStub(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := WriteInstanceSSHConfig("solo", 51010, "testuser", "/path/to/key"); err != nil {
		t.Fatalf("WriteInstanceSSHConfig() error = %v", err)
	}
	agg := readFile(t, ConfigPath())
	if !strings.Contains(agg, "Include ") {
		t.Errorf("aggregator stub missing Include:\n%s", agg)
	}
	if strings.Contains(agg, "Host bladerunner\n") {
		t.Errorf("aggregator stub must not invent a default-instance block:\n%s", agg)
	}
}

func TestWriteInstanceSSHConfigAppendsIncludeToLegacyAggregator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Simulate an aggregator written by an older bladerunner: one Host block,
	// no Include line.
	legacy := "Host bladerunner\n    Port 6022\n"
	if err := os.MkdirAll(Dir(), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(legacy), filePerm); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}

	if _, err := WriteInstanceSSHConfig("demo", 51020, "testuser", "/path/to/key"); err != nil {
		t.Fatalf("WriteInstanceSSHConfig() error = %v", err)
	}

	agg := readFile(t, ConfigPath())
	if !strings.HasPrefix(agg, legacy) {
		t.Errorf("legacy block must stay first and intact:\n%s", agg)
	}
	if !strings.Contains(agg, "Include ") {
		t.Errorf("Include was not appended:\n%s", agg)
	}
	// Appending twice must not duplicate the directive.
	if _, err := WriteInstanceSSHConfig("demo", 51021, "testuser", "/path/to/key"); err != nil {
		t.Fatalf("second WriteInstanceSSHConfig() error = %v", err)
	}
	if n := strings.Count(readFile(t, ConfigPath()), "Include "); n != 1 {
		t.Errorf("Include directive count = %d, want 1", n)
	}
}

func TestWriteConfigForDispatchesOnName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, name := range []string{"", DefaultInstanceName} {
		path, err := WriteConfigFor(name, 6022, "testuser", "/path/to/key")
		if err != nil {
			t.Fatalf("WriteConfigFor(%q) error = %v", name, err)
		}
		if path != ConfigPath() {
			t.Errorf("WriteConfigFor(%q) = %q, want the aggregator %q", name, path, ConfigPath())
		}
	}

	path, err := WriteConfigFor("demo", 51030, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteConfigFor(demo) error = %v", err)
	}
	if path != InstanceConfigPath("demo") {
		t.Errorf("WriteConfigFor(demo) = %q, want %q", path, InstanceConfigPath("demo"))
	}
}

func TestInstanceConfigPermissions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := WriteInstanceSSHConfig("demo", 51040, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteInstanceSSHConfig() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != filePerm {
		t.Errorf("instance config permissions = %o, want %o", mode, filePerm)
	}
}

func TestWriteInstanceSSHConfigRejectsBadNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, name := range []string{"", ".", "..", "../escape", "a/b", "star*", ".hidden"} {
		if _, err := WriteInstanceSSHConfig(name, 6022, "u", "/k"); err == nil {
			t.Errorf("WriteInstanceSSHConfig(%q) = nil error, want rejection", name)
		}
	}
}

func TestHostAliasAndCommand(t *testing.T) {
	if got := HostAlias(""); got != "bladerunner" {
		t.Errorf("HostAlias(\"\") = %q, want bladerunner", got)
	}
	if got := HostAlias(DefaultInstanceName); got != "bladerunner" {
		t.Errorf("HostAlias(default) = %q, want bladerunner", got)
	}
	if got := HostAlias("demo"); got != "bladerunner-demo" {
		t.Errorf("HostAlias(demo) = %q, want bladerunner-demo", got)
	}
	if got := Command("/tmp/cfg"); got != "ssh -F /tmp/cfg bladerunner" {
		t.Errorf("Command() = %q", got)
	}
	if got := CommandFor("/tmp/cfg", "demo"); got != "ssh -F /tmp/cfg bladerunner-demo" {
		t.Errorf("CommandFor() = %q", got)
	}
}

func TestAggregatorIncludeIsUnconditional(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	agg, err := WriteSSHConfig(6022, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteSSHConfig() error = %v", err)
	}
	got := readFile(t, agg)

	// An Include that follows a Host block belongs to that block and is only
	// processed when it matches; "Match all" reopens an unconditional context.
	// Without it the per-instance configs load only while connecting to the
	// default alias. See sshConfigResolves for the executable proof.
	matchIdx := strings.Index(got, "\nMatch all\n")
	if matchIdx < 0 {
		t.Fatalf("aggregator must precede the Include with 'Match all':\n%s", got)
	}
	hostIdx := strings.Index(got, "Host bladerunner\n")
	includeIdx := strings.Index(got, "\nInclude ")
	if hostIdx >= matchIdx || matchIdx >= includeIdx {
		t.Errorf("want Host block, then Match all, then Include; got %d/%d/%d\n%s",
			hostIdx, matchIdx, includeIdx, got)
	}
}

// sshPortKeyword is the "ssh -G" output key holding the resolved port.
const sshPortKeyword = "port"

// sshConfigResolves asks the real ssh client which port it would use for host,
// so the generated files are checked against ssh_config's actual semantics
// rather than our reading of them.
func sshConfigResolves(t *testing.T, configPath, host string) string {
	t.Helper()
	bin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh client not available")
	}
	out, err := exec.CommandContext(t.Context(), bin, "-F", configPath, "-G", host).Output()
	if err != nil {
		t.Fatalf("ssh -G %s: %v", host, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, sshPortKeyword+" "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("ssh -G %s printed no %s:\n%s", host, sshPortKeyword, out)
	return ""
}

func TestGeneratedConfigsResolveWithRealSSH(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	agg, err := WriteSSHConfig(6022, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteSSHConfig() error = %v", err)
	}
	inst, err := WriteInstanceSSHConfig("demo", 51001, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteInstanceSSHConfig() error = %v", err)
	}

	// The default instance keeps the bare alias even though an instance config
	// also declares it: the aggregator's own block is read first and wins.
	if got := sshConfigResolves(t, agg, "bladerunner"); got != "6022" {
		t.Errorf("ssh -F <aggregator> bladerunner => port %s, want 6022", got)
	}
	// The instance is reachable through the aggregator's Include.
	if got := sshConfigResolves(t, agg, "bladerunner-demo"); got != "51001" {
		t.Errorf("ssh -F <aggregator> bladerunner-demo => port %s, want 51001 (Include not processed?)", got)
	}
	// The CLI's fixed "bladerunner" alias still works against the instance's own
	// file, which is what `br shell` passes with -F.
	if got := sshConfigResolves(t, inst, "bladerunner"); got != "51001" {
		t.Errorf("ssh -F <instance> bladerunner => port %s, want 51001", got)
	}
	// Unrelated hosts are untouched.
	if got := sshConfigResolves(t, agg, "example.invalid"); got != "22" {
		t.Errorf("ssh -F <aggregator> example.invalid => port %s, want 22", got)
	}
}

// TestValidInstanceNameRejectsUnsafeNames pins the guard that stands between a
// derived instance name and a file written into the user's ssh config tree.
//
// The name reaching WriteConfigFor is NOT always checked by instance.ValidName.
// On the `br start --state-dir <path>` route, vmhost.Spec.Name is deliberately
// left empty (see buildStartSpec in cmd/bladerunner/start.go), Spec.validateIdentity
// therefore skips ValidName entirely, and Host.instanceName falls through to
// config.Config.InstanceName — which is filepath.Base of the state dir, with no
// validation of any kind. internal/vm.Runner.makeReport takes the same
// unvalidated value straight from cfg.InstanceName().
//
// So this guard is the ONLY check on that route. It must be an allowlist: the
// name is rendered unescaped by text/template into an ssh_config file, and
// interpolated into the shell command string CommandFor prints for the user to
// paste.
func TestValidInstanceNameRejectsUnsafeNames(t *testing.T) {
	unsafe := []struct{ name, why string }{
		{"a\rb", "carriage return is a line terminator to some config readers"},
		{"a\vb", "vertical tab is a control character"},
		{"a\fb", "form feed is a control character"},
		{"a#b", "# starts an ssh_config comment"},
		{"a=b", "= separates an ssh_config keyword from its value"},
		{"a;b", "; separates shell commands in the string CommandFor prints"},
		{"a|b", "| pipes in the string CommandFor prints"},
		{"a&b", "& backgrounds in the string CommandFor prints"},
		{"a$b", "$ expands in the string CommandFor prints"},
		{"a`b", "backtick substitutes a command in the string CommandFor prints"},
		{"a<b", "< redirects in the string CommandFor prints"},
		{"a>b", "> redirects in the string CommandFor prints"},
		{"a(b", "( subshells in the string CommandFor prints"},
		{"a~b", "~ expands to a home directory"},
	}
	for _, tc := range unsafe {
		if err := validInstanceName(tc.name); err == nil {
			t.Errorf("validInstanceName(%q) = nil, want an error: %s", tc.name, tc.why)
		}
	}
}

// TestValidInstanceNameAcceptsOrdinaryDirectoryNames guards the other side: the
// guard must keep accepting the names real instances already use, or tightening
// it would break working boots. It is deliberately WIDER than
// instance.ValidName — buildStartSpec documents that a slot basename may carry
// uppercase, an underscore, a dot, or more than instance.MaxNameLen characters,
// and those boots work today.
func TestValidInstanceNameAcceptsOrdinaryDirectoryNames(t *testing.T) {
	ok := []string{
		"demo", "my-vm", "vm2", "My_VM", "release.1", "a",
		strings.Repeat("x", 200),
	}
	for _, name := range ok {
		if err := validInstanceName(name); err != nil {
			t.Errorf("validInstanceName(%q) = %v, want nil", name, err)
		}
	}
}

// TestWriteInstanceSSHConfigRejectsUnsafeName is the end-to-end statement of the
// same rule: an unsafe name must not reach the filesystem at all.
func TestWriteInstanceSSHConfigRejectsUnsafeName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := WriteInstanceSSHConfig("evil#Port 2222", 51050, "u", "/k"); err == nil {
		t.Fatal("WriteInstanceSSHConfig() accepted a name containing an ssh_config comment character")
	}
	if entries, err := os.ReadDir(InstanceConfigDir()); err == nil && len(entries) != 0 {
		t.Errorf("rejected name still wrote %d file(s) into config.d", len(entries))
	}
}

// TestSSHGuardIsSupersetOfInstanceValidName pins the relationship between the
// two name rules so the layering stays coherent. Every name the authoritative
// validator accepts must also pass this package's guard — otherwise a properly
// registered instance would boot and then fail to get an ssh config.
//
// The converse does NOT hold, and must not: see validInstanceName's comment for
// the route on which instance.ValidName never runs at all.
func TestSSHGuardIsSupersetOfInstanceValidName(t *testing.T) {
	names := []string{"demo", "vm2", "a", "my-vm", "x0-9-z", strings.Repeat("n", 64)}
	for _, name := range names {
		if err := instance.ValidName(name); err != nil {
			t.Fatalf("test fixture %q is not a valid instance name: %v", name, err)
		}
		if err := validInstanceName(name); err != nil {
			t.Errorf("instance.ValidName accepts %q but the ssh guard rejects it: %v", name, err)
		}
	}
}
