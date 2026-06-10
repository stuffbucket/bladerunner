//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework UserNotifications
#include <stdlib.h>
#include "ui_bridge_darwin.h"
*/
import "C"

import (
	_ "embed"
	"sync"
	"time"
	"unsafe"
)

// splashIconICNS is the colorful app mark shown on the starting splash. The
// status-bar glyph (menubarGlyphPNG) is a black alpha mask for tinting and
// would look wrong on the HUD, so the splash uses the full app icon. NSImage
// decodes .icns directly. Regenerate via scripts/gen-brand-assets.sh.
//
//go:embed assets/AppIcon.icns
var splashIconICNS []byte

// splashMaxVisible is a safety auto-dismiss: a cold first boot can take minutes
// (image download/convert), but if boot stalls or fails the splash must never
// stick forever — the failure surfaces as a notification instead.
const splashMaxVisible = 5 * time.Minute

// cgoSplash is the real splashController: a borderless floating HUD window drawn
// by the Objective-C bridge. It is driven by the notification state machine
// (Show on Start, Hide on the first healthy edge). All AppKit work happens on
// the main thread inside the bridge (see runOnMain in ui_bridge_darwin.m).
type cgoSplash struct {
	mu    sync.Mutex
	timer *time.Timer
}

// defaultSplash returns the real cgo splash window. (Overrides the no-op stub
// declared for the pure-Go notification PR.)
func defaultSplash() splashController { return &cgoSplash{} }

func (s *cgoSplash) Show() {
	// C.CBytes copies the icon into C memory (unsafe.Pointer, never named — so
	// this file doesn't import unsafe). brShowSplash copies it again into an
	// NSData synchronously, so freeing right after the call is safe.
	if len(splashIconICNS) > 0 {
		cbytes := C.CBytes(splashIconICNS)
		C.brShowSplash(cbytes, C.int(len(splashIconICNS)))
		C.free(cbytes)
	} else {
		C.brShowSplash(nil, C.int(0))
	}

	// (Re)arm the safety auto-dismiss so a failed boot can't leave it stuck.
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(splashMaxVisible, func() { C.brHideSplash() })
	s.mu.Unlock()
}

func (s *cgoSplash) Hide() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	C.brHideSplash()
}

// unNotifier delivers branded banners via UNUserNotificationCenter. It is
// selected by defaultNotifier ONLY when running inside the signed Bladerunner
// .app bundle; outside the bundle UN can't deliver, so a no-op is used instead.
type unNotifier struct{}

func (unNotifier) notify(title, body string) {
	ctitle := C.CString(title)
	cbody := C.CString(body)
	defer C.free(unsafe.Pointer(ctitle))
	defer C.free(unsafe.Pointer(cbody))
	C.brPostNotification(ctitle, cbody)
}

// newUNNotifier requests notification authorization once and returns the
// UN-backed notifier.
func newUNNotifier() notifier {
	C.brRequestNotificationAuth()
	return unNotifier{}
}
