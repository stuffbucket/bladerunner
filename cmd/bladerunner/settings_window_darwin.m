#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include "settings_window_darwin.h"
#include "_cgo_export.h" // goSettingsSave (exported from Go)

// Local main-thread funnel (mirrors ui_bridge_darwin.m's; both have internal
// linkage so the duplicate name is fine). Every AppKit/WebKit call goes through
// it because the Go callers are background goroutines.
static void settingsRunOnMain(void (^block)(void)) {
  if ([NSThread isMainThread]) {
    block();
  } else {
    dispatch_async(dispatch_get_main_queue(), block);
  }
}

// BRSettingsBridge is both the WKWebView script-message handler (receives the
// form's JSON on Save) and the window delegate (restores the activation policy
// when the window closes).
@interface BRSettingsBridge : NSObject <WKScriptMessageHandler, NSWindowDelegate>
@end

@implementation BRSettingsBridge
- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
  if ([message.body isKindOfClass:[NSString class]]) {
    // Hand the posted JSON to Go (parse + validate + persist). Go calls back
    // via brSettingsMessage / brCloseSettings for the result.
    goSettingsSave((char *)[(NSString *)message.body UTF8String]);
  }
}

- (void)windowWillClose:(NSNotification *)notification {
  // The menubar is an LSUIElement accessory; drop back so it leaves the Dock /
  // app switcher once the settings window is gone.
  [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}
@end

static NSWindow *gSettingsWindow = nil;
static WKWebView *gSettingsWebView = nil;
static BRSettingsBridge *gSettingsBridge = nil;

void brShowSettings(const char *html) {
  NSString *h = html ? [NSString stringWithUTF8String:html] : @"";
  settingsRunOnMain(^{
    if (gSettingsWindow == nil) {
      NSRect frame = NSMakeRect(0, 0, 460, 580);
      gSettingsWindow = [[NSWindow alloc]
          initWithContentRect:frame
                    styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                               NSWindowStyleMaskResizable)
                      backing:NSBackingStoreBuffered
                        defer:NO];
      gSettingsWindow.title = @"Bladerunner Settings";
      gSettingsWindow.releasedWhenClosed = NO;

      gSettingsBridge = [[BRSettingsBridge alloc] init];
      gSettingsWindow.delegate = gSettingsBridge;

      WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
      [cfg.userContentController addScriptMessageHandler:gSettingsBridge
                                                    name:@"bladerunner"];
      gSettingsWebView = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
      gSettingsWindow.contentView = gSettingsWebView;
    }
    [gSettingsWebView loadHTMLString:h baseURL:nil];

    // A titled, focusable window needs the regular activation policy under an
    // accessory app, plus an explicit activate to come to front and accept
    // keyboard input. windowWillClose restores accessory.
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
    [NSApp activateIgnoringOtherApps:YES];
    [gSettingsWindow center];
    [gSettingsWindow makeKeyAndOrderFront:nil];
  });
}

void brCloseSettings(void) {
  settingsRunOnMain(^{
    if (gSettingsWindow != nil) {
      [gSettingsWindow close]; // triggers windowWillClose -> restore policy
    }
  });
}

void brSettingsMessage(const char *msg, int isError) {
  NSString *m = msg ? [NSString stringWithUTF8String:msg] : @"";
  settingsRunOnMain(^{
    if (gSettingsWebView == nil) {
      return;
    }
    // Encode the message as a JSON array element to get a safely-escaped JS
    // string literal, then call showMessage(<literal>, <bool>).
    NSData *d = [NSJSONSerialization dataWithJSONObject:@[ m ] options:0 error:nil];
    NSString *arr = [[NSString alloc] initWithData:d encoding:NSUTF8StringEncoding];
    NSString *js = [NSString stringWithFormat:@"showMessage(%@[0], %@)", arr,
                                              isError ? @"true" : @"false"];
    [gSettingsWebView evaluateJavaScript:js completionHandler:nil];
  });
}
