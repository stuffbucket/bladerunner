//go:build darwin

package main

import (
	"github.com/stuffbucket/bladerunner/internal/config"
)

// menubarTarget is the one instance the menubar reports on.
//
// The menubar used to read config.DefaultStateDir() directly in every probe, so
// it always described the DEFAULT VM regardless of what was actually running.
// That is wrong now in the flagship flow: the menubar's own cartridge watcher
// spawns a NAMED holder, whose state dir is the cartridge mountpoint, so after
// inserting a cartridge the menubar showed "Stopped" and offered Start — which
// would have booted a second, unrelated VM.
//
// The scope here is deliberately narrow. This is NOT a multi-VM menubar UI.
// The menu ACTIONS already behave correctly, because each one shells out to
// `br <verb>`, and `br` resolves its own target through resolveInstanceTarget:
// with one VM up it acts on that VM, with several it refuses. What was broken
// was only the menubar's READ path. So the fix is to route the read path
// through the same resolver, and to make "I cannot choose" a state the menu can
// actually display instead of silently rendering the wrong VM.
type menubarTarget struct {
	// stateDir is where this instance's control socket and boot-stage file
	// live. Empty when ambiguous.
	stateDir string
	// name is the instance name, for the status row.
	name string
	// isDefault marks the flat default instance, whose status row keeps its
	// original single-VM wording.
	isDefault bool
	// ambiguous records that several instances are running and none of them
	// was selected, so the menubar must not act on any.
	ambiguous bool
}

// resolveMenubarTarget picks the instance to report on using exactly the
// resolution policy the CLI uses, so the menubar and `br` can never disagree
// about which VM "the" VM is.
//
// Nothing running resolves to the flat default (unchanged single-VM behavior);
// exactly one running instance resolves to that one, named or not; several
// running instances resolve to ambiguous. A resolution error is only ever the
// ambiguous case here, because the menubar never names an instance.
func resolveMenubarTarget(s instanceScanner) menubarTarget {
	target, err := s.resolve(selectedInstanceName())
	if err != nil {
		return menubarTarget{ambiguous: true}
	}
	return menubarTarget{
		stateDir:  target.StateDir,
		name:      target.instanceName(),
		isDefault: target.isDefaultSlot(),
	}
}

// currentMenubarTarget resolves the live target against the real registry.
func currentMenubarTarget() menubarTarget {
	return resolveMenubarTarget(defaultScanner())
}

// menubarSettingsDir is the state dir the menubar reads and writes SETTINGS in.
//
// This one stays the default state dir on purpose, and is not a missed call
// site. Settings are per-user, not per-instance: internal/vmhost.Host loads them
// from the default state dir too (see the note at host.go's LoadSettings call),
// so a custom --state-dir slot has no settings file of its own. The same is true
// of the menubar's singleton socket (one menubar per user) and of the cartridge
// watcher's registry root (the root is where instances are listed FROM, not an
// instance itself).
func menubarSettingsDir() string { return config.DefaultStateDir() }
