package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/incus"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// instanceFlag is bound to the global --instance persistent flag (see root.go).
// It names which VM a verb acts on when more than one is running.
var instanceFlag string

const (
	// instanceEnvVar is the environment override for --instance. It follows the
	// repository's BLADERUNNER_* convention (BLADERUNNER_STATE_DIR,
	// BLADERUNNER_LOG_LEVEL); instanceEnvVarAlias is the shorter spelling,
	// accepted because the CLI itself is named "br".
	instanceEnvVar      = "BLADERUNNER_INSTANCE"
	instanceEnvVarAlias = "BR_INSTANCE"

	// defaultSlotAlias is the name `br eject` has always used for the flat
	// default instance. The registry records that same instance under
	// config.DefaultInstanceName, so both spellings select it.
	defaultSlotAlias = "default"

	// disksDirName is the legacy directory of disk slots under the state dir.
	disksDirName = "disks"

	// kindColumnWidth pads the Kind column of the ambiguity error so the port
	// summaries line up; it is the width of the longest instance.Kind.
	kindColumnWidth = 9
)

// selectedInstanceName returns the instance the user asked for, or "" when they
// did not ask for one. The flag wins over the environment.
func selectedInstanceName() string {
	if name := strings.TrimSpace(instanceFlag); name != "" {
		return name
	}
	if name := strings.TrimSpace(os.Getenv(instanceEnvVar)); name != "" {
		return name
	}
	return strings.TrimSpace(os.Getenv(instanceEnvVarAlias))
}

// resolvedInstance is one addressable VM: everything a verb needs in order to
// talk to it without re-deriving paths from config.DefaultStateDir().
type resolvedInstance struct {
	Name       string
	Kind       instance.Kind
	StateDir   string
	SourcePath string
	Mountpoint string
	PID        int
	Ports      instance.Ports
	StartedAt  time.Time
	Version    string

	// Running records that the control socket answered when the instance was
	// discovered. It is always true for an implicitly resolved instance.
	Running bool
	// Explicit records that the user named this instance, so a verb may report
	// "not running" instead of silently doing nothing.
	Explicit bool
	// Fallback records that nothing was running at all and this is the flat
	// default layout — exactly what every verb targeted before --instance
	// existed.
	Fallback bool
}

// isDefaultSlot reports whether this instance is the flat default layout rooted
// at the default state dir. Those verbs that offer to start the VM keep doing
// so only for that instance, so the single-VM UX is unchanged.
func (r resolvedInstance) isDefaultSlot() bool {
	return filepath.Clean(r.StateDir) == filepath.Clean(config.DefaultStateDir())
}

// instanceName returns the name this instance's ssh config and registry entry
// were written under.
//
// The registry name wins when there is one, because that is what the holder
// published and what it named its ssh config.d fragment. Deriving the name from
// the state directory is only a fallback for a legacy instance that has no
// entry — and it is WRONG for a cartridge, whose state dir is now
// /Volumes/bladerunner-<name>: the basename would be "bladerunner-demo" and the
// generated alias is "demo".
func (r resolvedInstance) instanceName() string {
	if r.Name != "" {
		return r.Name
	}
	cfg := &config.Config{VMDir: r.StateDir}
	return cfg.InstanceName()
}

// controlClient returns a control client for this instance. For the flat
// default it funnels through requireRunningVM, preserving the "the VM is not
// running, start it now?" prompt; a named instance is never auto-started.
func (r resolvedInstance) controlClient() (*control.Client, error) {
	if r.isDefaultSlot() {
		return requireRunningVM()
	}
	client := control.NewClient(r.StateDir)
	if !client.IsRunning() {
		return nil, fmt.Errorf("instance %q is not running", r.Name)
	}
	return client, nil
}

// instanceScanner discovers instances. The lookups are function fields so the
// resolution policy can be tested against a temporary state dir without a live
// control socket.
type instanceScanner struct {
	root    string
	running func(stateDir string) bool
	ports   func(stateDir string) instance.Ports
}

// defaultScanner is the scanner every verb uses: the real state dir, with
// liveness and ports read from the control socket.
func defaultScanner() instanceScanner {
	return instanceScanner{
		root:    config.DefaultStateDir(),
		running: func(stateDir string) bool { return control.NewClient(stateDir).IsRunning() },
		ports:   livePorts,
	}
}

// livePorts reads the host-side ports an instance published. It is best effort:
// an instance that does not answer (or speaks an older protocol) reports zeros
// rather than failing the listing.
func livePorts(stateDir string) instance.Ports {
	client := control.NewClient(stateDir)
	if !client.IsRunning() {
		return instance.Ports{}
	}
	port := func(k string) int {
		v, err := client.GetConfig(k)
		if err != nil {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		}
		return n
	}
	return instance.Ports{
		SSH:  port(control.ConfigKeyLocalSSHPort),
		API:  port(control.ConfigKeyLocalAPIPort),
		Web:  port(control.ConfigKeyLocalWebPort),
		OIDC: port(control.ConfigKeyLocalOIDCPort),
	}
}

// resolveInstanceTarget resolves the instance the current command acts on. This
// is the single entry point every verb uses instead of config.DefaultStateDir().
func resolveInstanceTarget() (resolvedInstance, error) {
	return defaultScanner().resolve(selectedInstanceName())
}

// targetStateDir resolves the selected instance's state dir. A resolution
// failure (an ambiguous selection, or an unknown name) is reported through the
// --json envelope as well, so callers can `return err` directly.
func targetStateDir() (string, error) {
	target, err := resolveInstanceTarget()
	if err != nil {
		return "", jsonOrError(err)
	}
	return target.StateDir, nil
}

// resolve applies the selection policy:
//
//   - an explicitly named instance always wins, running or not;
//   - exactly one running instance resolves implicitly (the single-VM case,
//     which is every existing install);
//   - nothing running resolves to the flat default layout, so verbs report
//     "not running" exactly as they always have;
//   - more than one running instance is an error that names the candidates.
func (s instanceScanner) resolve(name string) (resolvedInstance, error) {
	if name != "" {
		return s.resolveNamed(name)
	}
	running := s.runningInstances()
	switch len(running) {
	case 0:
		return resolvedInstance{
			Name:     config.DefaultInstanceName,
			Kind:     instance.KindFlat,
			StateDir: s.root,
			Fallback: true,
		}, nil
	case 1:
		return running[0], nil
	default:
		return resolvedInstance{}, s.ambiguousError(running)
	}
}

// resolveNamed resolves an explicitly named instance: the registry first, then
// the legacy layouts (the flat default, an attached cartridge, a disk slot) so
// a VM started by an older binary is still addressable by name.
func (s instanceScanner) resolveNamed(name string) (resolvedInstance, error) {
	if err := instance.ValidName(name); err != nil {
		return resolvedInstance{}, fmt.Errorf("instance %q: %w", name, err)
	}

	entry, err := instance.Read(s.root, name)
	switch {
	case err == nil:
		return s.mark(fromEntry(entry)), nil
	case !errors.Is(err, fs.ErrNotExist):
		logging.L().Debug("read instance entry", "name", name, "err", err)
	}

	if name == defaultSlotAlias || name == config.DefaultInstanceName {
		return s.mark(s.flatSlot()), nil
	}
	if mp, ok := attachedCartridgeMountpoint(s.root, name); ok {
		return s.mark(resolvedInstance{Name: name, Kind: instance.KindCartridge, StateDir: mp, Mountpoint: mp}), nil
	}
	if dir := filepath.Join(s.root, disksDirName, name); isDirectory(dir) {
		return s.mark(resolvedInstance{Name: name, Kind: instance.KindDisk, StateDir: dir}), nil
	}
	return resolvedInstance{}, s.unknownError(name)
}

// cartridgeMountCandidates lists every place a cartridge named name can be
// mounted on this host, in the order they should be tried:
//
//  1. the private slot <state>/mnt/<name>, used by scripted and headless boots
//     (and by every boot before mounting became browsable);
//  2. /Volumes/bladerunner-<name>, where macOS puts a browsable cartridge —
//     which is where a `br boot` puts one today.
//
// The second is a PREDICTION: macOS appends a collision suffix when the name is
// taken, so a cartridge can be mounted somewhere this list does not name. That
// is exactly why the registry is consulted first and this is only the fallback.
func cartridgeMountCandidates(root, name string) []string {
	return []string{
		cartridge.MountpointFor(root, name),
		cartridge.BrowsableMountpointFor(name),
	}
}

// attachedCartridgeMountpoint finds a cartridge of the given name that is
// mounted but has no registry entry.
func attachedCartridgeMountpoint(root, name string) (string, bool) {
	for _, mp := range cartridgeMountCandidates(root, name) {
		if cartridge.IsAttached(mp) {
			return mp, true
		}
	}
	return "", false
}

// mark stamps an explicitly selected instance with its liveness.
func (s instanceScanner) mark(r resolvedInstance) resolvedInstance {
	r.Explicit = true
	r.Running = s.running(r.StateDir)
	return r
}

// runningInstances returns every instance whose control socket answers: the
// registry unioned with a scan of the legacy directory layout, deduplicated by
// state dir and sorted by name.
//
// The legacy scan is not optional. internal/instance.List reads the registry
// only, so a VM started by an older binary (or one whose holder predates the
// registry) would otherwise be invisible to a freshly upgraded CLI — and an
// invisible VM cannot be stopped or ejected.
func (s instanceScanner) runningInstances() []resolvedInstance {
	seen := make(map[string]bool)
	out := make([]resolvedInstance, 0, 4)
	add := func(r resolvedInstance) {
		key := filepath.Clean(r.StateDir)
		if r.StateDir == "" || seen[key] || !s.running(r.StateDir) {
			return
		}
		seen[key] = true
		r.Running = true
		out = append(out, r)
	}

	entries, err := instance.List(s.root)
	if err != nil {
		logging.L().Debug("list instance registry", "err", err)
	}
	for i := range entries {
		add(fromEntry(entries[i]))
	}
	legacy := s.legacySlots()
	for i := range legacy {
		add(legacy[i])
	}

	slices.SortFunc(out, func(a, b resolvedInstance) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// legacySlots enumerates the pre-registry layout: the flat default rooted at the
// state dir, disk slots under <state>/disks/*, and cartridges attached under
// <state>/mnt/*. Liveness is not checked here; the caller filters.
func (s instanceScanner) legacySlots() []resolvedInstance {
	out := []resolvedInstance{s.flatSlot()}

	des, err := os.ReadDir(filepath.Join(s.root, disksDirName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.L().Debug("scan disk slots", "err", err)
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		out = append(out, resolvedInstance{
			Name:     de.Name(),
			Kind:     instance.KindDisk,
			StateDir: filepath.Join(s.root, disksDirName, de.Name()),
		})
	}

	for _, a := range cartridge.ListAttached(s.root) {
		out = append(out, resolvedInstance{
			Name:       a.Name,
			Kind:       instance.KindCartridge,
			StateDir:   a.Mountpoint,
			Mountpoint: a.Mountpoint,
		})
	}
	return out
}

// flatSlot describes the flat default instance.
func (s instanceScanner) flatSlot() resolvedInstance {
	return resolvedInstance{Name: config.DefaultInstanceName, Kind: instance.KindFlat, StateDir: s.root}
}

// fromEntry converts a registry record into an addressable instance.
func fromEntry(e instance.Entry) resolvedInstance {
	return resolvedInstance{
		Name:       e.Name,
		Kind:       e.Kind,
		StateDir:   e.StateDir,
		SourcePath: e.SourcePath,
		Mountpoint: e.Mountpoint,
		PID:        e.PID,
		Ports:      e.Ports,
		StartedAt:  e.StartedAt,
		Version:    e.BinaryVersion,
	}
}

// isDirectory reports whether path exists and is a directory.
func isDirectory(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// ambiguousError reports that the selection is ambiguous and lists every
// candidate with its kind and published ports, so the user can pick one without
// running another command.
func (s instanceScanner) ambiguousError(running []resolvedInstance) error {
	var b strings.Builder
	b.WriteString("multiple instances running; select one with --instance <name>:")
	width := 0
	for i := range running {
		width = max(width, len(running[i].Name))
	}
	for i := range running {
		r := &running[i]
		fmt.Fprintf(&b, "\n  %-*s  %-*s  %s", width, r.Name, kindColumnWidth, r.Kind, s.portSummary(r))
	}
	return errors.New(b.String())
}

// unknownError reports that a named instance could not be found, listing what
// is running instead.
func (s instanceScanner) unknownError(name string) error {
	running := s.runningInstances()
	if len(running) == 0 {
		return fmt.Errorf("unknown instance %q (nothing is running)", name)
	}
	names := make([]string, 0, len(running))
	for i := range running {
		names = append(names, running[i].Name)
	}
	return fmt.Errorf("unknown instance %q; running: %s", name, strings.Join(names, ", "))
}

// portSummary renders the host-side ports of an instance, filling them in from
// the control socket when the record does not carry them (a legacy instance
// predates the registry). Used only on the reporting paths.
func (s instanceScanner) portSummary(r *resolvedInstance) string {
	ports := r.Ports
	if ports == (instance.Ports{}) {
		ports = s.ports(r.StateDir)
	}
	var parts []string
	for _, p := range []struct {
		label string
		port  int
	}{{"ssh", ports.SSH}, {"api", ports.API}} {
		if p.port > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p.label, p.port))
		}
	}
	if len(parts) == 0 {
		return "no published ports"
	}
	return strings.Join(parts, "  ")
}

// --- ssh / incus targeting ------------------------------------------------

// sshTarget resolves the ssh config path and instance name the CLI should use
// to reach the selected VM. The returned name feeds ssh.HostAlias, so a named
// instance is reached through its own config.d fragment and alias.
func sshTarget() (configPath, instanceName string, err error) {
	r, err := resolveInstanceTarget()
	if err != nil {
		return "", "", err
	}
	if r.isDefaultSlot() {
		// Unchanged path for the single-VM install, including the offer to
		// start the VM when it is not running.
		path, cerr := sshConfigFromControl()
		if cerr != nil {
			return "", "", cerr
		}
		return path, config.DefaultInstanceName, nil
	}
	client, err := r.controlClient()
	if err != nil {
		return "", "", err
	}
	path, err := client.GetConfig(control.ConfigKeySSHConfigPath)
	if err != nil {
		logging.L().Debug("get ssh config path failed", "instance", r.Name, "err", err)
		return "", "", fmt.Errorf("instance %q: %w", r.Name, errVMNotRunning)
	}
	return path, r.instanceName(), nil
}

// incusClientForTarget dials the Incus API of the selected instance, taking the
// port and the client certificate from that same instance.
func incusClientForTarget() (*incus.Client, error) {
	r, err := resolveInstanceTarget()
	if err != nil {
		return nil, err
	}
	if r.isDefaultSlot() {
		return connectIncus()
	}
	client, err := r.controlClient()
	if err != nil {
		return nil, err
	}
	return incusClientFromControl(client, r.StateDir)
}

// --- `br instances` -------------------------------------------------------

var instancesCmd = &cobra.Command{
	Use:   "instances",
	Short: "List running VM instances",
	Long: `List the bladerunner VM instances that are currently running, with the
ports they published and the process holding each one.

Any verb can be pointed at one of these with --instance <name> (or the
BLADERUNNER_INSTANCE environment variable). With a single VM running there is
nothing to choose and --instance can be omitted.

Registry entries left behind by a holder process that died are pruned as a side
effect of listing.`,
	Args: cobra.NoArgs,
	RunE: runInstances,
}

// instanceListing is one row of `br instances`, and the element type of its
// --json array.
type instanceListing struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	StateDir   string         `json:"state_dir"`
	Running    bool           `json:"running"`
	PID        int            `json:"pid,omitempty"`
	Ports      instance.Ports `json:"ports"`
	StartedAt  string         `json:"started_at,omitempty"`
	Uptime     string         `json:"uptime,omitempty"`
	SourcePath string         `json:"source_path,omitempty"`
	Mountpoint string         `json:"mountpoint,omitempty"`
	Version    string         `json:"binary_version,omitempty"`
}

func runInstances(_ *cobra.Command, _ []string) error {
	scanner := defaultScanner()
	if removed, err := instance.Prune(scanner.root); err != nil {
		logging.L().Debug("prune instance registry", "err", err)
	} else if len(removed) > 0 {
		logging.L().Debug("pruned dead instance entries", "names", removed)
	}

	listings := scanner.listings(scanner.runningInstances())
	if jsonOutput {
		return emitJSON(listings)
	}
	return renderInstanceListings(os.Stdout, listings)
}

// listings builds the reporting view of the running instances, filling in ports
// that the record itself does not carry. It always returns a non-nil slice so
// zero instances marshal as `[]` rather than `null`.
func (s instanceScanner) listings(running []resolvedInstance) []instanceListing {
	out := make([]instanceListing, 0, len(running))
	for i := range running {
		r := &running[i]
		ports := r.Ports
		if ports == (instance.Ports{}) {
			ports = s.ports(r.StateDir)
		}
		l := instanceListing{
			Name:       r.Name,
			Kind:       string(r.Kind),
			StateDir:   r.StateDir,
			Running:    r.Running,
			PID:        r.PID,
			Ports:      ports,
			SourcePath: r.SourcePath,
			Mountpoint: r.Mountpoint,
			Version:    r.Version,
		}
		if !r.StartedAt.IsZero() {
			l.StartedAt = r.StartedAt.Format(time.RFC3339)
			l.Uptime = time.Since(r.StartedAt).Round(time.Second).String()
		}
		out = append(out, l)
	}
	return out
}

// renderInstanceListings writes the human table for `br instances`.
func renderInstanceListings(out io.Writer, listings []instanceListing) error {
	if len(listings) == 0 {
		fmt.Fprintln(out, subtle("No VM instances are running. Start one with 'br start'."))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tKIND\tSSH\tAPI\tUPTIME\tPID\tSTATE DIR\tSOURCE"); err != nil {
		return err
	}
	for i := range listings {
		l := &listings[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			l.Name, l.Kind,
			portCell(l.Ports.SSH), portCell(l.Ports.API),
			emptyCell(l.Uptime), emptyCell(pidCell(l.PID)),
			l.StateDir, emptyCell(l.SourcePath),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// missingCell is what a table cell shows when the value is unknown (an instance
// started before the registry recorded it, say).
const missingCell = "-"

func portCell(port int) string {
	if port <= 0 {
		return missingCell
	}
	return strconv.Itoa(port)
}

func pidCell(pid int) string {
	if pid <= 0 {
		return ""
	}
	return strconv.Itoa(pid)
}

func emptyCell(s string) string {
	if s == "" {
		return missingCell
	}
	return s
}
