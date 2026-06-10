#ifndef BR_UI_BRIDGE_DARWIN_H
#define BR_UI_BRIDGE_DARWIN_H

// brShowSplash shows the borderless "bladerunner is starting…" splash: a
// centered, floating, vibrant HUD panel with the app icon and an indeterminate
// spinner. iconData/iconLen is an image (any NSImage-decodable format, e.g.
// .icns/.png); pass NULL/0 to leave the icon unchanged. The bytes are copied
// synchronously before this returns, so the caller may free them immediately.
// Safe to call from any goroutine: it marshals onto the main thread.
void brShowSplash(const void *iconData, int iconLen);

// brHideSplash hides the splash if shown. Safe to call from any goroutine.
void brHideSplash(void);

#endif // BR_UI_BRIDGE_DARWIN_H
