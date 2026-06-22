#ifndef BR_UI_BRIDGE_DARWIN_H
#define BR_UI_BRIDGE_DARWIN_H

// brShowSplash shows the borderless, floating, vibrant HUD splash: the
// multi-line ASCII `banner` (the CLI's "bladerunner" figlet) rendered in the
// brand gradient with a shimmer sweeping left-to-right across the glyphs, and a
// loading line underneath. Safe to call from any goroutine: it marshals onto
// the main thread.
void brShowSplash(const char *banner);

// brHideSplash hides the splash if shown. Safe to call from any goroutine.
void brHideSplash(void);

// brSetSplashStatus updates the status line beneath the banner with a short,
// human-friendly phase string ("Booting Linux…", "Starting Incus…", "Ready").
// No-op if the splash isn't built or the string is empty. Safe to call from any
// goroutine.
void brSetSplashStatus(const char *status);

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
