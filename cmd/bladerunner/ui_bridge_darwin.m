#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>
#import <UserNotifications/UserNotifications.h>
#include "ui_bridge_darwin.h"

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

// brandGradientColors is the purple->green ramp shared with the CLI banner.
static NSArray *brandGradientColors(void) {
  return @[
    (id)[NSColor colorWithSRGBRed:0.541 green:0.361 blue:0.965 alpha:1].CGColor, // #8A5CF6
    (id)[NSColor colorWithSRGBRed:0.345 green:0.396 blue:0.949 alpha:1].CGColor, // #5865F2
    (id)[NSColor colorWithSRGBRed:0.231 green:0.510 blue:0.965 alpha:1].CGColor, // #3B82F6
    (id)[NSColor colorWithSRGBRed:0.024 green:0.714 blue:0.831 alpha:1].CGColor, // #06B6D4
    (id)[NSColor colorWithSRGBRed:0.063 green:0.725 blue:0.506 alpha:1].CGColor, // #10B981
    (id)[NSColor colorWithSRGBRed:0.204 green:0.827 blue:0.600 alpha:1].CGColor, // #34D399
  ];
}

// bannerMaskLayer builds a CATextLayer of the multi-line ASCII banner, used as
// an alpha mask so a gradient only shows through the glyph pixels. White text =
// opaque mask. A fresh instance is needed per gradient (a layer can't share a
// mask).
static CATextLayer *bannerMaskLayer(NSString *banner, NSFont *font, CGRect frame, CGFloat scale) {
  CATextLayer *t = [CATextLayer layer];
  t.string = banner;
  t.font = (__bridge CFTypeRef)font;
  t.fontSize = font.pointSize;
  t.foregroundColor = NSColor.whiteColor.CGColor;
  t.wrapped = YES;
  t.alignmentMode = kCAAlignmentLeft;
  t.frame = frame;
  t.contentsScale = scale;
  return t;
}

// buildSplashWindow constructs the borderless HUD once: the banner rendered in
// the brand gradient with a left-to-right shimmer sweeping across the glyphs,
// and a loading line beneath. Sized to the measured banner.
static NSWindow *buildSplashWindow(NSString *banner) {
  NSFont *mono = [NSFont monospacedSystemFontOfSize:19 weight:NSFontWeightBold];
  NSRect tb = [banner boundingRectWithSize:NSMakeSize(8000, 8000)
                                   options:NSStringDrawingUsesLineFragmentOrigin
                                attributes:@{NSFontAttributeName : mono}];
  CGFloat textW = ceil(tb.size.width);
  CGFloat textH = ceil(tb.size.height);

  const CGFloat padX = 64, padTop = 44, gap = 28, loadingH = 22, padBottom = 38;
  CGFloat W = textW + padX * 2;
  CGFloat H = padTop + textH + gap + loadingH + padBottom;
  NSRect frame = NSMakeRect(0, 0, W, H);

  NSWindow *win = [[NSWindow alloc] initWithContentRect:frame
                                              styleMask:NSWindowStyleMaskBorderless
                                                backing:NSBackingStoreBuffered
                                                  defer:NO];
  win.level = NSFloatingWindowLevel;
  win.opaque = NO;
  win.backgroundColor = NSColor.clearColor;
  win.hasShadow = YES;
  win.releasedWhenClosed = NO;
  // Show across Spaces and over fullscreen apps; never steal key focus (the
  // splash is informational and the app runs as an LSUIElement accessory).
  win.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                           NSWindowCollectionBehaviorFullScreenAuxiliary;
  win.ignoresMouseEvents = YES;

  // Solid near-black panel (matching the brand's dark squircle), rounded.
  NSView *bg = [[NSView alloc] initWithFrame:frame];
  bg.wantsLayer = YES;
  bg.layer.backgroundColor =
      [NSColor colorWithSRGBRed:0.043 green:0.059 blue:0.078 alpha:1.0].CGColor; // #0B0F14
  bg.layer.cornerRadius = 22.0;
  bg.layer.masksToBounds = YES;
  win.contentView = bg;

  CGFloat scale = NSScreen.mainScreen.backingScaleFactor > 0
                      ? NSScreen.mainScreen.backingScaleFactor
                      : 2.0;

  // Banner host layer, centered horizontally, near the top (layers are y-up).
  CALayer *host = [CALayer layer];
  host.frame = CGRectMake((W - textW) / 2.0, H - padTop - textH, textW, textH);
  [bg.layer addSublayer:host];
  CGRect local = CGRectMake(0, 0, textW, textH);

  // Base: the brand gradient, masked to the glyphs -> the colored "bladerunner".
  CAGradientLayer *base = [CAGradientLayer layer];
  base.frame = local;
  base.colors = brandGradientColors();
  base.startPoint = CGPointMake(0, 0.5);
  base.endPoint = CGPointMake(1, 0.5);
  base.mask = bannerMaskLayer(banner, mono, local, scale);
  [host addSublayer:base];

  // Shimmer: a bright band, masked to the same glyphs, whose stops animate
  // left-to-right so a highlight sweeps across the figlet text.
  CAGradientLayer *shine = [CAGradientLayer layer];
  shine.frame = local;
  shine.colors = @[
    (id)[NSColor colorWithWhite:1 alpha:0].CGColor,
    (id)[NSColor colorWithWhite:1 alpha:0.85].CGColor,
    (id)[NSColor colorWithWhite:1 alpha:0].CGColor,
  ];
  shine.startPoint = CGPointMake(0, 0.5);
  shine.endPoint = CGPointMake(1, 0.5);
  shine.locations = @[ @0, @0.5, @1 ];
  shine.mask = bannerMaskLayer(banner, mono, local, scale);
  [host addSublayer:shine];

  CABasicAnimation *anim = [CABasicAnimation animationWithKeyPath:@"locations"];
  anim.fromValue = @[ @(-0.5), @(-0.25), @0 ];
  anim.toValue = @[ @1.0, @1.25, @1.5 ];
  anim.duration = 1.7;
  anim.repeatCount = HUGE_VALF;
  anim.timingFunction =
      [CAMediaTimingFunction functionWithName:kCAMediaTimingFunctionEaseInEaseOut];
  [shine addAnimation:anim forKey:@"shimmer"];

  // Loading line beneath the banner, centered across the full width.
  NSTextField *label =
      [[NSTextField alloc] initWithFrame:NSMakeRect(0, padBottom, W, loadingH)];
  label.editable = NO;
  label.selectable = NO;
  label.bezeled = NO;
  label.bordered = NO;
  label.drawsBackground = NO;
  label.alignment = NSTextAlignmentCenter;
  label.textColor = [NSColor colorWithWhite:0.92 alpha:1.0];
  label.font = [NSFont systemFontOfSize:15 weight:NSFontWeightRegular];
  label.stringValue = @"Starting…";
  [bg addSubview:label];

  return win;
}

void brShowSplash(const char *banner) {
  NSString *b = banner ? [NSString stringWithUTF8String:banner] : @"";
  runOnMain(^{
    if (gSplashWindow == nil) {
      gSplashWindow = buildSplashWindow(b);
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
