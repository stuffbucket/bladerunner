//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework UserNotifications
#include <stdlib.h>
#include "ui_bridge_darwin.h"
*/
import "C"

import (
	"sync"
	"time"
	"unsafe"

	"github.com/stuffbucket/bladerunner/internal/ui"
)

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
	// The splash renders the CLI "bladerunner" figlet banner with a shimmer; the
	// bridge copies the string synchronously, so freeing right after is safe.
	cbanner := C.CString(ui.BannerPlain())
	C.brShowSplash(cbanner)
	C.free(unsafe.Pointer(cbanner))

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
