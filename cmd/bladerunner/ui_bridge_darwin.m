#import <Cocoa/Cocoa.h>
#import <UserNotifications/UserNotifications.h>
#include "ui_bridge_darwin.h"

// View tags so brShowSplash can find and update the icon/message subviews when
// the panel already exists.
enum {
  kSplashIconTag = 1001,
  kSplashLabelTag = 1002,
};

// The single splash window, retained for the process lifetime (reused across
// show/hide). All access is on the main thread via runOnMain.
static NSWindow *gSplashWindow = nil;

// runOnMain funnels every AppKit call onto the main thread. The Go callers
// (health poll / click goroutines) are never the main thread, and AppKit off
// the main thread is undefined (intermittent crashes/hangs) — this is the one
// place the rule is enforced. Already-on-main runs inline so a synchronous
// caller isn't deferred a runloop turn.
static void runOnMain(void (^block)(void)) {
  if ([NSThread isMainThread]) {
    block();
  } else {
    dispatch_async(dispatch_get_main_queue(), block);
  }
}

// buildSplashWindow constructs the borderless HUD panel once. Layout is fixed
// (the content is tiny and static), so manual frames are simpler than
// constraints here.
static NSWindow *buildSplashWindow(void) {
  NSRect frame = NSMakeRect(0, 0, 240, 150);
  NSWindow *win = [[NSWindow alloc] initWithContentRect:frame
                                             styleMask:NSWindowStyleMaskBorderless
                                               backing:NSBackingStoreBuffered
                                                 defer:NO];
  win.level = NSFloatingWindowLevel;
  win.opaque = NO;
  win.backgroundColor = [NSColor clearColor];
  win.hasShadow = YES;
  win.releasedWhenClosed = NO;
  // Show across Spaces and over fullscreen apps; never steal key focus (the
  // splash is informational and the app runs as an LSUIElement accessory).
  win.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                           NSWindowCollectionBehaviorFullScreenAuxiliary;
  win.ignoresMouseEvents = YES;

  NSVisualEffectView *bg = [[NSVisualEffectView alloc] initWithFrame:frame];
  bg.material = NSVisualEffectMaterialHUDWindow;
  bg.blendingMode = NSVisualEffectBlendingModeBehindWindow;
  bg.state = NSVisualEffectStateActive;
  bg.wantsLayer = YES;
  bg.layer.cornerRadius = 16.0;
  bg.layer.masksToBounds = YES;
  win.contentView = bg;

  NSImageView *icon = [[NSImageView alloc] initWithFrame:NSMakeRect(96, 78, 48, 48)];
  icon.tag = kSplashIconTag;
  icon.imageScaling = NSImageScaleProportionallyUpOrDown;
  [bg addSubview:icon];

  NSTextField *label = [[NSTextField alloc] initWithFrame:NSMakeRect(12, 44, 216, 22)];
  label.tag = kSplashLabelTag;
  label.editable = NO;
  label.selectable = NO;
  label.bezeled = NO;
  label.bordered = NO;
  label.drawsBackground = NO;
  label.alignment = NSTextAlignmentCenter;
  label.textColor = [NSColor labelColor];
  label.font = [NSFont systemFontOfSize:13 weight:NSFontWeightMedium];
  label.stringValue = @"bladerunner is starting…";
  [bg addSubview:label];

  NSProgressIndicator *spinner =
      [[NSProgressIndicator alloc] initWithFrame:NSMakeRect(112, 16, 16, 16)];
  spinner.style = NSProgressIndicatorStyleSpinning;
  spinner.indeterminate = YES;
  [spinner startAnimation:nil];
  [bg addSubview:spinner];

  return win;
}

void brShowSplash(const void *iconData, int iconLen) {
  // Copy the icon bytes into an NSData synchronously (before the async hop) so
  // the Go caller can free its buffer the moment this returns.
  NSData *icon = (iconData && iconLen > 0)
                     ? [NSData dataWithBytes:iconData length:(NSUInteger)iconLen]
                     : nil;
  runOnMain(^{
    if (gSplashWindow == nil) {
      gSplashWindow = buildSplashWindow();
    }
    if (icon != nil) {
      NSImage *img = [[NSImage alloc] initWithData:icon];
      NSView *content = gSplashWindow.contentView;
      ((NSImageView *)[content viewWithTag:kSplashIconTag]).image = img;
    }
    [gSplashWindow center];
    [gSplashWindow orderFrontRegardless];
  });
}

void brHideSplash(void) {
  runOnMain(^{
    if (gSplashWindow != nil) {
      [gSplashWindow orderOut:nil];
    }
  });
}

void brRequestNotificationAuth(void) {
  runOnMain(^{
    @try {
      UNUserNotificationCenter *center =
          [UNUserNotificationCenter currentNotificationCenter];
      [center requestAuthorizationWithOptions:(UNAuthorizationOptionAlert |
                                               UNAuthorizationOptionSound)
                            completionHandler:^(BOOL granted, NSError *error) {
                              (void)granted;
                              (void)error; // denial degrades to no banners
                            }];
    } @catch (NSException *e) {
      // currentNotificationCenter raises for a process without a valid bundle
      // id + code signature (e.g. a bare `br menubar` or an unsigned dev
      // bundle). Swallow: notifications simply won't appear there.
      (void)e;
    }
  });
}

void brPostNotification(const char *title, const char *body) {
  NSString *t = title ? [NSString stringWithUTF8String:title] : @"";
  NSString *b = body ? [NSString stringWithUTF8String:body] : @"";
  runOnMain(^{
    @try {
      UNUserNotificationCenter *center =
          [UNUserNotificationCenter currentNotificationCenter];
      UNMutableNotificationContent *content =
          [[UNMutableNotificationContent alloc] init];
      content.title = t;
      content.body = b;
      content.sound = [UNNotificationSound defaultSound];
      // nil trigger => deliver immediately. A fresh UUID id per request so
      // banners never coalesce/replace one another.
      UNNotificationRequest *req =
          [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString]
                                               content:content
                                               trigger:nil];
      [center addNotificationRequest:req withCompletionHandler:nil];
    } @catch (NSException *e) {
      (void)e;
    }
  });
}
