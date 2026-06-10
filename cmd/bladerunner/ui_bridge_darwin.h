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

// brRequestNotificationAuth asks the user once for permission to post
// notifications (UNUserNotificationCenter). Only meaningful inside a signed,
// bundled app; guarded with @try/@catch so a misconfigured/unsigned bundle
// degrades to a no-op instead of raising. Safe to call from any goroutine.
void brRequestNotificationAuth(void);

// brPostNotification posts a banner with the given title/body via
// UNUserNotificationCenter, so it shows the app's icon + name and respects the
// per-app notification settings. No-op (swallowed) if the center is
// unavailable. Safe to call from any goroutine.
void brPostNotification(const char *title, const char *body);

#endif // BR_UI_BRIDGE_DARWIN_H
