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
	"github.com/stuffbucket/bladerunner/internal/util"
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

	// Protection is what the holder recorded about this instance's
	// DiskArbitration unmount veto. It is only meaningful for a cartridge, and
	// only a registry entry carries it: an instance resolved from the legacy
	// directory layout has no record, which reads as
	// instance.ProtectionUnrecorded rather than as "protected".
	Protection instance.Protection

	// Liveness is where this instance sat on internal/instance's ladder when it
	// was discovered: Serving (its control socket accepted a connection),
	// ProcessOnly (no socket, but a live holder process is recorded for it), or
	// Dead. It is deliberately not a boolean — the middle rung is what tells a
	// wedged instance apart from one that is really gone.
	Liveness instance.Liveness
	// Explicit records that the user named this instance, so a verb may report
	// "not running" instead of silently doing nothing.
	Explicit bool
	// Fallback records that nothing was running at all and this is the flat
	// default layout — exactly what every verb targeted before --instance
	// existed.
	Fallback bool
}

// isLive reports whether anything still holds this instance: either it is
// serving or a live holder process is recorded for it. This is the rung a
// data-safety guard needs — a wedged holder still has the disk image open.
func (r resolvedInstance) isLive() bool {
	return r.Liveness != instance.Dead
}

// isServing reports whether this instance's control socket accepted a
// connection. Note that this is weaker than "will answer a request": a wedged
// holder accepts and never replies.
func (r resolvedInstance) isServing() bool {
	return r.Liveness == instance.Serving
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

// instanceScanner discovers instances. The lookups are function fields so the
// resolution policy can be tested against a temporary state dir without a live
// control socket.
type instanceScanner struct {
	root string
	// liveness places one candidate on the ladder. It is NOT a ping: resolution
	// has to find a holder that is alive but wedged, and a ping cannot tell that
	// apart from a holder that is gone. See cmd/bladerunner/liveness.go.
	liveness func(r resolvedInstance) instance.Liveness
	ports    func(stateDir string) instance.Ports
}

// defaultScanner is the scanner every verb uses: the real state dir, with
// liveness read from the control socket and the start lock, and ports read from
// the control socket.
func defaultScanner() instanceScanner {
	return instanceScanner{
		root:     config.DefaultStateDir(),
		liveness: livenessOf,
		ports:    livePorts,
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

// requireRunningTarget resolves the instance the user selected (--instance,
// BLADERUNNER_INSTANCE, or the single running VM) and returns a control client
// for it.
//
// This is the choke point: every verb that acts on a running VM goes through
// here, so --instance is honored in one place instead of being re-derived from
// config.DefaultStateDir() in each of them (issue #9).
func requireRunningTarget() (*control.Client, resolvedInstance, error) {
	target, err := resolveInstanceTarget()
	if err != nil {
		return nil, resolvedInstance{}, err
	}
	client, err := requireRunningVM(target)
	if err != nil {
		return nil, target, err
	}
	return client, target, nil
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

// requireDefaultInstance resolves the selection for a verb that can only act on
// the flat default, and refuses an explicit selection of anything else.
//
// `br restore` and `br upgrade` both hand the instance to runStart, which
// rebuilds the flat default's specification only: a disk slot needs its
// manifest and a cartridge needs its image, and 'br boot' is what carries
// those. Acting on the default while the user named another instance is the
// defect; refusing and naming the verbs that do work is the honest answer.
func requireDefaultInstance(verb string) (string, error) {
	target, err := resolveInstanceTarget()
	if err != nil {
		return "", err
	}
	if target.Explicit && !target.isDefaultSlot() {
		name := target.instanceName()
		return "", fmt.Errorf("'br %s' acts on the default instance only, not the %s instance %q; "+
			"stop that one with 'br eject %s' and bring it back with 'br boot %s'",
			verb, target.Kind, name, name, name)
	}
	return config.DefaultStateDir(), nil
}

// resolve applies the selection policy:
//
//   - an explicitly named instance always wins, running or not;
//   - exactly one live instance resolves implicitly (the single-VM case,
//     which is every existing install);
//   - nothing live resolves to the flat default layout, so verbs report
//     "not running" exactly as they always have;
//   - more than one live instance is an error that names the candidates.
//
// "Live" is the liveness ladder, not a ping. This used to filter candidates by
// a ping round trip, which meant a holder that was alive but wedged survived no
// filter at all: nothing answered, so the resolver fell through to the flat
// default with PID 0 and never read the registry entry that knew the answer.
// The bare `br stop --force` that every unresponsive-VM message suggests then
// acted on the wrong instance, and the wedged one could only be reached by a
// user who already knew its name and typed --instance (issue #290).
//
// Candidates are taken from the strongest rung that has any. That keeps the
// ambiguity error honest: a Serving instance is proof of a live listener,
// whereas ProcessOnly rests on a recorded PID that the OS may since have
// recycled, and a phantom of that kind must not make an otherwise unambiguous
// selection ambiguous.
func (s instanceScanner) resolve(name string) (resolvedInstance, error) {
	if name != "" {
		return s.resolveNamed(name)
	}
	candidates := s.liveInstances()
	if serving := servingOnly(candidates); len(serving) > 0 {
		candidates = serving
	}
	switch len(candidates) {
	case 0:
		return resolvedInstance{
			Name:     config.DefaultInstanceName,
			Kind:     instance.KindFlat,
			StateDir: s.root,
			Fallback: true,
		}, nil
	case 1:
		return candidates[0], nil
	default:
		return resolvedInstance{}, s.ambiguousError(candidates)
	}
}

// servingOnly narrows a candidate list to the instances whose control socket
// accepted a connection.
func servingOnly(candidates []resolvedInstance) []resolvedInstance {
	out := make([]resolvedInstance, 0, len(candidates))
	for i := range candidates {
		if candidates[i].isServing() {
			out = append(out, candidates[i])
		}
	}
	return out
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
	if dir := filepath.Join(s.root, disksDirName, name); util.DirExists(dir) {
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
	r.Liveness = s.liveness(r)
	return r
}

// liveInstances returns every instance that something still holds — serving on
// its control socket, or with a live holder process recorded for it: the
// registry unioned with a scan of the legacy directory layout, deduplicated by
// state dir and sorted by name.
//
// The legacy scan is not optional. internal/instance.List reads the registry
// only, so a VM started by an older binary (or one whose holder predates the
// registry) would otherwise be invisible to a freshly upgraded CLI — and an
// invisible VM cannot be stopped or ejected.
func (s instanceScanner) liveInstances() []resolvedInstance {
	seen := make(map[string]bool)
	out := make([]resolvedInstance, 0, 4)
	add := func(r resolvedInstance) {
		key := filepath.Clean(r.StateDir)
		if r.StateDir == "" || seen[key] {
			return
		}
		r.Liveness = s.liveness(r)
		if !r.isLive() {
			return
		}
		seen[key] = true
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
		Protection: e.UnmountProtection,
	}
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
	running := s.liveInstances()
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
	client, target, err := requireRunningTarget()
	if err != nil {
		return "", "", err
	}
	path, err := client.GetConfig(control.ConfigKeySSHConfigPath)
	if err != nil {
		logging.L().Debug("get ssh config path failed", "instance", target.instanceName(), "err", err)
		return "", "", notRunningError(target)
	}
	return path, target.instanceName(), nil
}

// incusClientForTarget dials the Incus API of the selected instance, taking the
// port and the client certificate from that same instance.
func incusClientForTarget() (*incus.Client, error) {
	client, target, err := requireRunningTarget()
	if err != nil {
		return nil, err
	}
	return incusClientFromControl(client, target)
}

// --- `br instances` -------------------------------------------------------

var instancesCmd = &cobra.Command{
	Use:   "instances",
	Short: "List running VM instances",
	// colima spells this `list`. Note `br ls` is a DIFFERENT thing: Incus
	// instances inside the guest, not the VMs themselves. The names are close
	// and the meanings are not, which is why both are in the grouped help.
	Aliases: []string{"list"},
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

	// UnmountProtection says whether ejecting this cartridge spins the guest
	// down in an orderly way, and when it does not, why. It is nil — and so
	// absent from the JSON — for an instance with no cartridge to protect.
	UnmountProtection *unmountProtectionReport `json:"unmount_protection,omitempty"`
}

func runInstances(_ *cobra.Command, _ []string) error {
	scanner := defaultScanner()
	if removed, err := instance.Prune(scanner.root); err != nil {
		logging.L().Debug("prune instance registry", "err", err)
	} else if len(removed) > 0 {
		logging.L().Debug("pruned dead instance entries", "names", removed)
	}

	listings := scanner.listings(scanner.liveInstances())
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
			Name:              r.Name,
			Kind:              string(r.Kind),
			StateDir:          r.StateDir,
			Running:           r.isLive(),
			PID:               r.PID,
			Ports:             ports,
			SourcePath:        r.SourcePath,
			Mountpoint:        r.Mountpoint,
			Version:           r.Version,
			UnmountProtection: protectionReportFor(r.Kind, r.Protection),
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
//
// The EJECT column is the one piece of state that is not merely descriptive:
// it says whether pulling this cartridge out in Finder shuts the guest down
// first. It stays a single word so the table survives, and the reason a
// cartridge is not protected goes underneath it — see writeProtectionNotes.
func renderInstanceListings(out io.Writer, listings []instanceListing) error {
	if len(listings) == 0 {
		// Name the verbs that actually bring an instance back, and how to find
		// out what there is to bring back. This used to say "Start one with 'br
		// start'", which is the defect notRunningError fixed in vmgate.go: for a
		// disk or a cartridge, 'br start' creates an ADDITIONAL flat VM rather
		// than booting the instance the user meant, so following the advice
		// leaves two VMs where one was wanted and the original still down.
		fmt.Fprintln(out, subtle("No VM instances are running.\n"+
			"  start the default VM with 'br up', or boot a disk or cartridge with 'br boot <name>'\n"+
			"  'br disks' lists what you can boot"))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tKIND\tSSH\tAPI\tUPTIME\tPID\tEJECT\tSTATE DIR\tSOURCE"); err != nil {
		return err
	}
	for i := range listings {
		l := &listings[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			l.Name, l.Kind,
			portCell(l.Ports.SSH), portCell(l.Ports.API),
			emptyCell(l.Uptime), emptyCell(pidCell(l.PID)),
			protectionCell(l.UnmountProtection),
			l.StateDir, emptyCell(l.SourcePath),
		); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeProtectionNotes(out, listings)
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
