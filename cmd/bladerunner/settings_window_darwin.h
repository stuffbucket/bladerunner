#ifndef BR_SETTINGS_WINDOW_DARWIN_H
#define BR_SETTINGS_WINDOW_DARWIN_H

// brShowSettings creates (once) and shows the settings window hosting a
// WKWebView loaded with the given HTML form. It bumps the app to a regular
// (focusable) activation policy while open and restores accessory on close.
// Safe to call from any goroutine.
void brShowSettings(const char *html);

// brCloseSettings closes the settings window (restoring the accessory policy).
void brCloseSettings(void);

// brSettingsMessage shows a message in the form's status line; isError != 0
// renders it as an error, otherwise as an informational notice.
void brSettingsMessage(const char *msg, int isError);

#endif // BR_SETTINGS_WINDOW_DARWIN_H
