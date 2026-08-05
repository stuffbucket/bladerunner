package imagebuild

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// StepKind classifies a build step. Three kinds cover the whole recipe, and
// every mechanic knows how to perform all three: the native chroot runs them
// against a mounted root, and the appliance lowers them into virt-customize
// arguments. Keeping the vocabulary this small is what lets one recipe drive
// mechanics that share no code.
type StepKind string

const (
	// StepRun executes Argv inside the guest root.
	StepRun StepKind = "run"
	// StepWriteFile creates or replaces Path with Content at Mode.
	StepWriteFile StepKind = "write-file"
	// StepAppendFile appends Content to Path, creating it when absent.
	StepAppendFile StepKind = "append-file"
)

// Guest-side paths the build writes that are not owned by internal/provision.
const (
	// aptRetriesConfPath makes apt retry a transient mirror or CDN reset. A
	// single connection reset on one .deb has failed an entire architecture's
	// build while the other succeeded, so this is not optional polish.
	aptRetriesConfPath = "/etc/apt/apt.conf.d/80-bladerunner-retries"
	// initramfsModulesPath lists modules to include in the initramfs.
	initramfsModulesPath = "/etc/initramfs-tools/modules"
	// aptListsGlob is the downloaded package index, dropped so the baked image
	// does not ship a stale one.
	aptListsGlob = "/var/lib/apt/lists/*"
)

// aptRetries is how many times apt retries a failed fetch.
const aptRetries = 5

// Modes for the files the build writes directly. Both are read by tooling, not
// executed, so neither needs the executable bit.
const (
	aptConfMode      fs.FileMode = 0o644
	versionStampMode fs.FileMode = 0o644
	initramfsMode    fs.FileMode = 0o644
)

// timeSyncPackage is the time daemon whose presence decides whether masking
// systemd-timesyncd is safe. Masking it without a replacement would leave the
// guest with no time sync at all, which is the failure wake-heal exists to fix.
const timeSyncPackage = "chrony"

// Step is one unit of work in a guest image build. It is plain data so the
// sequence can be asserted in a test without a mounted image, root, or a
// hypervisor — the ordering constraints between these steps are the part most
// likely to break silently, and the part hardest to see in a shell script.
type Step struct {
	// Kind selects how the step is performed.
	Kind StepKind
	// Desc is a human-readable summary, logged before the step runs and
	// included when it fails.
	Desc string
	// Argv is the command and its arguments, for StepRun.
	Argv []string
	// Path is the absolute guest-side destination, for the file kinds.
	Path string
	// Mode is the file mode to apply, for StepWriteFile.
	Mode fs.FileMode
	// Content is the file body, for the file kinds.
	Content string
	// Optional marks a step whose failure must not fail the build.
	//
	// It is for work that depends on something outside the distribution's own
	// archive, where an outage is an inconvenience rather than a defect in the
	// image. A skipped step is reported, never swallowed.
	Optional bool
}

// Steps lowers the recipe into the ordered sequence of actions that produces the
// image.
//
// The order carries real dependencies, and every one of them fails silently
// rather than loudly when inverted: packages must be installed before their
// config files are written, because the package creates the directory; units
// must exist before they are enabled; the initramfs must be regenerated after
// the module list is extended and before the apt cache it needs is dropped. The
// sequence is returned as data precisely so a test can assert those pairs.
func (r Recipe) Steps() []Step {
	steps := []Step{
		{
			Kind: StepWriteFile,
			Desc: "configure apt to retry transient fetch failures",
			Path: aptRetriesConfPath,
			Mode: aptConfMode,
			// Written before the first apt call so the retry budget also covers
			// the index fetch, which is itself a network operation that fails.
			Content: fmt.Sprintf("Acquire::Retries %q;\n", fmt.Sprint(aptRetries)),
		},
		{
			Kind: StepRun,
			Desc: "refresh the package index",
			Argv: []string{"apt-get", "update"},
		},
		{
			Kind: StepRun,
			Desc: fmt.Sprintf("install %d packages: %s", len(r.Packages), strings.Join(r.Packages, ", ")),
			Argv: append([]string{"apt-get", "install", "-y"}, r.Packages...),
		},
	}

	// Assets land after the install because their destination directories are
	// created by the packages: /etc/chrony does not exist until chrony is in.
	for _, a := range r.Assets {
		steps = append(steps, Step{
			Kind:    StepWriteFile,
			Desc:    fmt.Sprintf("install %s", a.GuestPath),
			Path:    a.GuestPath,
			Mode:    a.Mode,
			Content: a.Content,
		})
	}

	for _, unit := range r.EnableUnits {
		steps = append(steps, Step{
			Kind: StepRun,
			Desc: fmt.Sprintf("enable %s", unit),
			Argv: []string{"systemctl", "enable", unit},
		})
	}

	steps = append(steps, maskTimeSyncSteps(r)...)
	steps = append(steps, webUISteps()...)

	steps = append(steps,
		Step{
			Kind: StepAppendFile,
			Desc: fmt.Sprintf("add %d initramfs modules", len(r.InitramfsModules)),
			Path: initramfsModulesPath,
			Mode: initramfsMode,
			// Without these the guest cannot bring up vsock, so every host relay
			// fails at boot with no obvious cause inside the guest.
			Content: linesOf(r.InitramfsModules),
		},
		Step{
			Kind: StepWriteFile,
			Desc: fmt.Sprintf("stamp the image version %s", r.Version),
			Path: r.VersionPath,
			Mode: versionStampMode,
			// No trailing newline: this matches what the shell build has always
			// written, so a Go-built image is byte-identical to the published
			// ones here. Readers trim, so the choice is invisible at runtime —
			// but the parity check compares bytes, and a difference it cannot
			// explain is worse than a convention it does not follow.
			Content: r.Version,
		},
		Step{
			Kind: StepRun,
			Desc: "regenerate the initramfs with the new modules",
			Argv: []string{"update-initramfs", "-u"},
		},
	)

	return append(steps, cleanupSteps()...)
}

// maskTimeSyncSteps disables systemd-timesyncd, but only when the recipe
// actually installs a replacement.
//
// The guard is on the package set rather than on a runtime check because an
// offline root has no running systemd: `systemctl is-active chrony` is always
// false there, so guarding on it would mask timesyncd unconditionally and ship
// guests with no time sync whatsoever. Tying the step to the recipe means
// dropping chrony from Packages removes the mask automatically.
func maskTimeSyncSteps(r Recipe) []Step {
	if !slices.Contains(r.Packages, timeSyncPackage) {
		return nil
	}
	return []Step{{
		Kind: StepRun,
		Desc: "mask systemd-timesyncd now that chrony provides time sync",
		// Tolerant of both sub-commands: on an image where timesyncd was never
		// present, disable and mask are no-ops that still report failure, and
		// that is not a reason to fail the build.
		Argv: []string{"/bin/sh", "-c",
			"systemctl disable systemd-timesyncd || true; systemctl mask systemd-timesyncd || true"},
	}}
}

// cleanupSteps removes the build's own apt scaffolding, so the baked image is
// the image and not the image plus the toolmarks that made it.
func cleanupSteps() []Step {
	return []Step{
		{
			Kind: StepRun,
			Desc: "drop the apt package cache",
			Argv: []string{"apt-get", "clean"},
		},
		{
			Kind: StepRun,
			Desc: "drop the downloaded package index",
			// A glob needs a shell; the index is a directory of many files whose
			// names are not known ahead of time.
			Argv: []string{"/bin/sh", "-c", "rm -rf " + aptListsGlob},
		},
		{
			Kind: StepRun,
			Desc: "remove the build-time apt retry configuration",
			Argv: []string{"rm", "-f", aptRetriesConfPath},
		},
	}
}

// linesOf renders values one per line, each newline-terminated, which is the
// form both files this build appends to expect.
func linesOf(values []string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return b.String()
}
