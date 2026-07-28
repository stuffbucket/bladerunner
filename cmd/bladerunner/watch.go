package main

// `br watch` — the headless half of goal 4.
//
// The menubar notices a cartridge being inserted and offers to boot it. Not
// everyone runs the menubar (and nobody runs it in CI), so the same watcher is
// available as a foreground verb that prints what it sees and either prompts or
// boots outright.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

var watchFlags struct {
	auto bool
	once bool
}

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Notice cartridges being inserted and offer to boot them",
	Long: `Watch for bladerunner cartridges being mounted and offer to boot each one.

A cartridge is a single file holding one whole VM, built by 'br disk pack': the
runnable .sparseimage, or the compressed .dmg that --ship produces for AirDrop.
AirDrop one to this Mac (or double-click it) and macOS mounts it; this command
notices the mount, checks that the volume really is a bootable cartridge, and
asks whether to boot it. On accept the read-only view is unmounted and a holder
process is started with the image file itself, so the shipped artifact stays
pristine.

Volumes already held by a running instance are ignored — a booted cartridge's
own mount looks exactly like a fresh insertion — and a cartridge that cannot be
booted (missing root.img, packed by a newer bladerunner, unreadable) is reported
with the reason rather than passed over in silence.

Cartridges already mounted when the watch starts are reported too, so nothing is
missed by starting late.

  --yes     Boot every cartridge found, without asking (alias: --auto)
  --once    Report what is mounted right now and exit

Requires macOS: mount detection is DiskArbitration.`,
	Args:    cobra.NoArgs,
	RunE:    runWatch,
	GroupID: groupMedia,
}

func init() {
	f := watchCmd.Flags()
	f.BoolVarP(&watchFlags.auto, "yes", "y", false, "Boot a detected cartridge without asking")
	// --auto is the same switch under the name the menubar-less/automation
	// audience reaches for first. Both write the one variable, so passing
	// either (or both) turns it on.
	f.BoolVar(&watchFlags.auto, "auto", false, "Alias for --yes")
	f.BoolVar(&watchFlags.once, "once", false, "Report the cartridges mounted right now and exit")

	// root.go's init registers the groups and runs first (Go orders a package's
	// init functions by file name), so GroupID above is already known here.
	rootCmd.AddCommand(watchCmd)
}

func runWatch(cmd *cobra.Command, _ []string) error {
	sink := &watchReporter{auto: watchFlags.auto, json: jsonOutput, out: os.Stdout}
	watcher := newCartridgeWatcher(config.DefaultStateDir(), sink.handle)

	if watchFlags.once {
		disks, err := diskarb.CurrentDisks()
		if err != nil {
			return jsonOrError(fmt.Errorf("list mounted volumes: %w", err))
		}
		watcher.catchUp(disks)
		sink.summarize()
		return nil
	}

	stop, err := startCartridgeWatch(watcher)
	if err != nil {
		return jsonOrError(err)
	}
	defer stop()

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	sink.announceWatching()
	<-ctx.Done()
	return nil
}

// watchReporter renders decisions and drives the accept path for `br watch`.
//
// It runs SYNCHRONOUSLY on the DiskArbitration queue, which is deliberate here
// and only here: `br watch` is a dedicated foreground process whose whole job
// is this one question, so serializing on the prompt is exactly right — it
// keeps two cartridges inserted at once from interleaving their prompts on the
// same terminal. (The menubar must not do this; see watchCartridgesForMenubar.)
type watchReporter struct {
	auto bool
	json bool
	out  io.Writer

	// reported counts the volumes that produced any output, so --once can say
	// "nothing found" instead of printing nothing at all.
	reported int
}

// handle reports one decision and, for an offer, resolves it.
func (r *watchReporter) handle(a watchAction) {
	r.emit(a)
	if a.Verdict != verdictOffer {
		return
	}
	if !r.accept(a) {
		r.emit(a.outcome(verdictDeclined, 0, nil))
		return
	}
	pid, err := bootDetectedCartridge(a)
	if err != nil {
		r.emit(a.outcome(verdictFailed, 0, err))
		return
	}
	r.emit(a.outcome(verdictBooted, pid, nil))
}

// emit writes one action, as a JSON object under --json (one per event, so the
// stream stays consumable while the watch is still running) and as a line
// otherwise. Both forms go to the reporter's own writer — the JSON envelope is
// the same shape emitJSON produces, but a long-lived watch emits many of them
// rather than one result at the end.
func (r *watchReporter) emit(a watchAction) {
	r.reported++
	if r.json {
		enc := json.NewEncoder(r.out)
		enc.SetIndent("", jsonIndent)
		if err := enc.Encode(a); err != nil {
			logging.L().Debug("emit watch event", "err", err)
		}
		return
	}
	fmt.Fprintf(r.out, "%s %s\n", watchGlyph(a.Verdict), a.describe())
}

// jsonIndent matches emitJSON's indentation so `br watch --json` looks like
// every other JSON the CLI produces.
const jsonIndent = "  "

// watchGlyph is the line marker for a verdict.
func watchGlyph(v watchVerdict) string {
	switch v {
	case verdictBooted:
		return success("✓")
	case verdictWarn, verdictFailed:
		return warning("⚠")
	case verdictOffer, verdictDeclined, verdictIgnore:
		return subtle("›")
	default:
		return subtle("›")
	}
}

// accept decides whether to boot an offered cartridge: --yes/--auto always,
// never under --json or without a terminal to ask at, otherwise by asking.
func (r *watchReporter) accept(a watchAction) bool {
	if r.auto {
		return true
	}
	if r.json {
		return false // machine mode: report, never act unasked
	}
	if !interactiveTerminal() {
		fmt.Fprintf(r.out, "  %s\n", subtle("not a terminal; re-run with --yes to boot it"))
		return false
	}
	fmt.Fprintf(r.out, "%s Boot cartridge %s now? %s ", subtle("›"), value(a.Name), subtle("[Y/n]"))
	return confirmStartVMFrom(os.Stdin)
}

// announceWatching prints the one-line "I am watching" preamble.
func (r *watchReporter) announceWatching() {
	if r.json {
		return
	}
	fmt.Fprintf(r.out, "%s Watching for cartridges. %s\n",
		subtle("›"), subtle("Insert one, or press Ctrl-C to stop."))
}

// summarize closes out a --once run that found nothing worth reporting.
func (r *watchReporter) summarize() {
	if r.json || r.reported > 0 {
		return
	}
	fmt.Fprintln(r.out, subtle("No bladerunner cartridges are mounted."))
}
