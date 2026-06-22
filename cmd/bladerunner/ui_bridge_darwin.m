#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>
#import <UserNotifications/UserNotifications.h>
#include "ui_bridge_darwin.h"

// The single splash window, retained for the process lifetime (reused across
// show/hide). All access is on the main thread via runOnMain.
static NSWindow *gSplashWindow = nil;
// The shimmer band layer, swept once per show by startSplashShimmer.
static CAGradientLayer *gShineLayer = nil;

// startSplashShimmer runs the highlight sweep exactly once, beginning one
// second after the splash appears (the band rests off-screen the rest of the
// time). Re-armed on each show.
static void startSplashShimmer(void) {
  if (gShineLayer == nil) {
    return;
  }
  [gShineLayer removeAnimationForKey:@"shimmer"];
  CABasicAnimation *anim = [CABasicAnimation animationWithKeyPath:@"locations"];
  anim.fromValue = @[ @(-0.5), @(-0.25), @0 ];
  anim.toValue = @[ @1.0, @1.25, @1.5 ]; // ends at the resting (off-right) model
  anim.duration = 1.3;
  anim.beginTime = CACurrentMediaTime() + 1.0; // after 1s on screen
  anim.timingFunction =
      [CAMediaTimingFunction functionWithName:kCAMediaTimingFunctionEaseInEaseOut];
  [gShineLayer addAnimation:anim forKey:@"shimmer"];
}

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

// brandGradientColors is the vaporwave ramp shared with the CLI banner:
// ultraviolet -> ice-cyan -> magenta/pink. The cyan<->magenta pivot is the
// vaporwave signature; the near-black panel keeps it serious.
static NSArray *brandGradientColors(void) {
  return @[
    (id)[NSColor colorWithSRGBRed:0.694 green:0.310 blue:1.000 alpha:1].CGColor, // #B14FFF
    (id)[NSColor colorWithSRGBRed:0.431 green:0.357 blue:1.000 alpha:1].CGColor, // #6E5BFF
    (id)[NSColor colorWithSRGBRed:0.169 green:0.831 blue:1.000 alpha:1].CGColor, // #2BD4FF
    (id)[NSColor colorWithSRGBRed:0.133 green:0.827 blue:0.933 alpha:1].CGColor, // #22D3EE
    (id)[NSColor colorWithSRGBRed:1.000 green:0.310 blue:0.847 alpha:1].CGColor, // #FF4FD8
    (id)[NSColor colorWithSRGBRed:1.000 green:0.435 blue:0.639 alpha:1].CGColor, // #FF6FA3
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

  CGFloat scale = NSScreen.mainScreen.backingScaleFactor > 0
                      ? NSScreen.mainScreen.backingScaleFactor
                      : 2.0;

  // Solid near-black panel (brand dark squircle), rounded, with a hairline edge.
  // Elevation is the window's rounded drop shadow (hasShadow + the transparent
  // clipped corners shape it).
  NSView *bg = [[NSView alloc] initWithFrame:frame];
  bg.wantsLayer = YES;
  bg.layer.backgroundColor =
      [NSColor colorWithSRGBRed:0.051 green:0.067 blue:0.090 alpha:1.0].CGColor; // ~#0D1117
  bg.layer.cornerRadius = 22.0;
  bg.layer.borderWidth = 1.0;
  bg.layer.borderColor = [NSColor colorWithWhite:1.0 alpha:0.14].CGColor;
  bg.layer.masksToBounds = YES;
  bg.layer.contentsScale = scale;
  win.contentView = bg;

  // Banner host layer, centered horizontally, near the top (layers are y-up).
  CALayer *host = [CALayer layer];
  host.frame = CGRectMake((W - textW) / 2.0, H - padTop - textH, textW, textH);
  host.contentsScale = scale;
  [bg.layer addSublayer:host];
  CGRect local = CGRectMake(0, 0, textW, textH);

  // Base: the brand gradient, masked to the glyphs -> the colored "bladerunner".
  // contentsScale on every layer keeps the masked text crisp on Retina (no
  // blurry-PNG look — this is vector text, not an image).
  CAGradientLayer *base = [CAGradientLayer layer];
  base.frame = local;
  base.contentsScale = scale;
  base.colors = brandGradientColors();
  base.startPoint = CGPointMake(0, 0.5);
  base.endPoint = CGPointMake(1, 0.5);
  base.mask = bannerMaskLayer(banner, mono, local, scale);
  [host addSublayer:base];

  // Shimmer: a bright band masked to the same glyphs. Its RESTING position is
  // off the right edge (invisible); startSplashShimmer sweeps it across exactly
  // once, a second after the splash appears, then it rests off-screen again.
  CAGradientLayer *shine = [CAGradientLayer layer];
  shine.frame = local;
  shine.contentsScale = scale;
  shine.colors = @[
    (id)[NSColor colorWithWhite:1 alpha:0].CGColor,
    (id)[NSColor colorWithWhite:1 alpha:0.9].CGColor,
    (id)[NSColor colorWithWhite:1 alpha:0].CGColor,
  ];
  shine.startPoint = CGPointMake(0, 0.5);
  shine.endPoint = CGPointMake(1, 0.5);
  shine.locations = @[ @1.0, @1.25, @1.5 ]; // off-right => no visible shine at rest
  shine.mask = bannerMaskLayer(banner, mono, local, scale);
  [host addSublayer:shine];
  gShineLayer = shine;

  // Loading line beneath the banner — monospaced + wide tracking + dimmed, so it
  // reads as a status label that belongs with the figlet rather than a plain
  // system string.
  NSTextField *label =
      [[NSTextField alloc] initWithFrame:NSMakeRect(0, padBottom, W, loadingH)];
  label.editable = NO;
  label.selectable = NO;
  label.bezeled = NO;
  label.bordered = NO;
  label.drawsBackground = NO;
  label.alignment = NSTextAlignmentCenter;
  NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
  para.alignment = NSTextAlignmentCenter;
  label.attributedStringValue = [[NSAttributedString alloc]
      initWithString:@"STARTING"
          attributes:@{
            NSFontAttributeName : [NSFont monospacedSystemFontOfSize:11
                                                              weight:NSFontWeightSemibold],
            NSForegroundColorAttributeName : [NSColor colorWithWhite:0.60 alpha:1.0],
            NSKernAttributeName : @4.5,
            NSParagraphStyleAttributeName : para,
          }];
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
    startSplashShimmer(); // one sweep, 1s after it appears
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
