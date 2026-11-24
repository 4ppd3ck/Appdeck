//go:build darwin && cgo

// Native launcher window on macOS: a real Cocoa window hosting WKWebView —
// the macOS counterpart of the Windows WebView2 window (native_windows.go).
// No browser is involved: the dashboard gets its own Dock icon (the runtime
// activation policy overrides the bundle's LSUIElement agent setting) and
// its own window; closing the window returns to Go, which the launcher
// treats as "quit the application". Built only when cgo is enabled
// (CGO_ENABLED=1 + Xcode command line tools); otherwise the stub in
// native_other.go falls back to the Edge/Chrome app window.
package main

/*
#cgo CFLAGS: -x objective-c -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <string.h>

// Provided by native_darwin_quit.go (//export appdeckRequestQuit):
// hands every AppKit quit path straight to the Go shutdown.
extern void appdeckRequestQuit(void);

// Provided by tray_darwin.go (//export): the menu bar companion's data and
// actions. The menu rebuilds from Go's cached snapshot (menuNeedsUpdate: —
// main thread, no IO); actions hop to Go goroutines so clicks never block.
extern char *appdeckTrayMenuJSON(void);
extern void appdeckTraySearch(void);
extern void appdeckTrayAppAction(const char *id, const char *url, int running);
extern void appdeckTrayToggle(const char *id, int pause);
extern int appdeckTrayRunningOnlyToggle(void);

// Menu bar companion state. gDeckKeepRunning flips the close-button
// contract: with the tray installed, closing the dashboard window hides it
// (the deck keeps running in the menu bar) instead of quitting — quit moves
// to the Quit item / ⌘Q, which still lands in applicationShouldTerminate:.
@class DeckTrayDelegate;
static int gDeckKeepRunning = 0;
static NSStatusItem *gDeckTray = nil;
static DeckTrayDelegate *gDeckTrayDelegate = nil;
static NSMenu *gDeckTrayMenu = nil; // the status item's menu (rebuilds on open)
static NSWindow *gDeckMainWindow = nil;
static WKWebView *gDeckMainWebView = nil;

// Forward declaration: the popup path below (createWebViewWithConfiguration)
// registers app windows with the transparency tracker defined further down.
static void deckAddWindow(NSWindow *w);
static char *deckTrayStrDup(const char *s);


// MacRunDelegate owns the window lifecycle. Every quit path — ⌘Q, the Quit
// menu item, and the red close button — is routed through
// applicationShouldTerminate:, which CANCELS AppKit's terminate (a plain
// terminate would exit() the process from Objective-C and skip the Go
// shutdown that stops the launched apps) and instead stops the NSApp run
// loop. [app run] then returns to Go, which runs the same beginShutdown
// contract as window close on Windows: kill the apps, then exit.
@interface MacRunDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate>
@property (assign) BOOL shuttingDown;
@end

@implementation MacRunDelegate
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)app {
    return gDeckKeepRunning ? NO : YES; // with the tray, closing just hides
}
// The red close button (and ⌘W): with the tray, the window hides and the
// deck keeps running in the menu bar; without it, the old behavior — the
// close routes through applicationShouldTerminate: and quits.
- (BOOL)windowShouldClose:(NSWindow *)window {
    if (!gDeckKeepRunning) return YES;
    [window orderOut:nil];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    return NO;
}
- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)app {
    if (self.shuttingDown) return NSTerminateNow;
    self.shuttingDown = YES;
    // Deterministic quit: call into Go right here so the launched apps are
    // stopped and the process exits (os.Exit inside beginShutdown) even if
    // the run-loop unwind below were to misfire. The stop+postEvent dance is
    // only the fallback that returns [NSApp run] to Go if it is ever needed.
    appdeckRequestQuit();
    [app stop:nil];
    NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                       location:NSMakePoint(0, 0)
                                  modifierFlags:0
                                      timestamp:0
                                   windowNumber:0
                                        context:nil
                                        subtype:0
                                          data1:0
                                          data2:0];
    [app postEvent:wake atStart:YES];
    return NSTerminateCancel;
}
@end

// MacJSDelegate owns the webview callbacks: the JS panels (confirm/alert),
// file picks, AND navigation + downloads. WKWebView silently drops
// attachment navigations unless a navigation delegate answers with the
// .download policy and a WKDownloadDelegate provides a destination — every
// app page's browser-mode download (RepoLens's asset downloads) was dead in
// the window without this. Files land in ~/Downloads under the
// server-suggested name, uniquified, no prompt (a browser's silent save).
// Download settings, pushed from Go (nativeConfigureDownloads): the folder
// browser-mode downloads land in (nil = ~/Downloads), and whether each one
// should ask for a destination first.
static NSString *gDeckDownloadDir = nil;
static int gDeckDownloadAsk = 0;
void deckSetDownloadDir(const char *path) {
    gDeckDownloadDir = (path && *path) ? [[NSString alloc] initWithUTF8String:path] : nil;
}
void deckSetDownloadAsk(int ask) { gDeckDownloadAsk = ask ? 1 : 0; }

// One tracked download: the WKDownload plus where its progress goes. The
// dashboard renders the strip (window.__deckDownload); an app page opened in
// its own window has no such hook and simply gets the browser behaviour.
@interface DeckDownloadItem : NSObject
@property (nonatomic, retain) WKDownload *download;
@property (nonatomic, retain) WKWebView *webView;
@property (nonatomic, copy) NSString *name;
@property (nonatomic, copy) NSString *destPath;
@property (nonatomic, assign) NSUInteger ident;
@property (nonatomic, assign) NSTimeInterval lastPush;
@end
@implementation DeckDownloadItem
@end

static NSMutableArray *gDeckDownloads = nil; // DeckDownloadItem, live only
static NSUInteger gDeckDownloadSeq = 0;
static void *deckDownloadKVO = &deckDownloadKVO;

// deckJSQuote escapes a value for a single-quoted JavaScript string literal.
static NSString *deckJSQuote(NSString *s) {
    if (!s) return @"";
    NSMutableString *out = [NSMutableString stringWithString:s];
    [out replaceOccurrencesOfString:@"\\" withString:@"\\\\" options:0 range:NSMakeRange(0, out.length)];
    [out replaceOccurrencesOfString:@"'" withString:@"\\'" options:0 range:NSMakeRange(0, out.length)];
    [out replaceOccurrencesOfString:@"\n" withString:@" " options:0 range:NSMakeRange(0, out.length)];
    [out replaceOccurrencesOfString:@"\r" withString:@" " options:0 range:NSMakeRange(0, out.length)];
    return out;
}

// deckPushDownload reports one download's state to the dashboard's strip.
// Progress events are throttled — WebKit reports every chunk.
static void deckPushDownload(DeckDownloadItem *item, NSString *state, NSString *extra) {
    if (!item) return;
    NSTimeInterval now = [NSDate timeIntervalSinceReferenceDate];
    if ([state isEqualToString:@"progress"] && now - item.lastPush < 0.15) return;
    item.lastPush = now;
    WKWebView *wv = item.webView;
    if (!wv) return;
    NSProgress *p = item.download.progress;
    double pct = p ? p.fractionCompleted * 100.0 : 0.0;
    long long done = p ? p.completedUnitCount : 0;
    long long total = p ? p.totalUnitCount : 0;
    NSString *js = [NSString stringWithFormat:
        @"window.__deckDownload&&window.__deckDownload({id:%lu,name:'%@',state:'%@',pct:%0.1f,done:%lld,total:%lld%@})",
        (unsigned long)item.ident, deckJSQuote(item.name), state, pct, done, total, extra ? extra : @""];
    [wv evaluateJavaScript:js completionHandler:nil];
}

static DeckDownloadItem *deckItemForDownload(WKDownload *download) {
    for (DeckDownloadItem *it in gDeckDownloads) {
        if (it.download == download) return it;
    }
    return nil;
}

static DeckDownloadItem *deckItemForIdent(NSString *ident) {
    for (DeckDownloadItem *it in gDeckDownloads) {
        if ([[NSString stringWithFormat:@"%lu", (unsigned long)it.ident] isEqualToString:ident]) return it;
    }
    return nil;
}

// deckDownloadDestination: the configured folder (or ~/Downloads), with the
// suggested name uniquified so a second copy does not overwrite the first.
static NSString *deckDownloadDestination(NSString *suggested) {
    NSString *dir = gDeckDownloadDir;
    if (dir.length == 0) {
        NSArray<NSString *> *dirs = NSSearchPathForDirectoriesInDomains(NSDownloadsDirectory, NSUserDomainMask, YES);
        dir = dirs.firstObject;
    }
    if (!dir) return nil;
    NSString *base = suggested.length ? suggested : @"download";
    NSString *ext = base.pathExtension;
    NSString *stem = ext.length ? [base stringByDeletingPathExtension] : base;
    NSString *candidate = [dir stringByAppendingPathComponent:base];
    for (int i = 2; [[NSFileManager defaultManager] fileExistsAtPath:candidate]; i++) {
        candidate = ext.length
            ? [dir stringByAppendingPathComponent:[NSString stringWithFormat:@"%@ (%d).%@", stem, i, ext]]
            : [dir stringByAppendingPathComponent:[NSString stringWithFormat:@"%@ (%d)", stem, i]];
    }
    return candidate;
}

@interface MacJSDelegate : NSObject <WKUIDelegate, WKNavigationDelegate, WKDownloadDelegate>
@end

@implementation MacJSDelegate
// window.open() from the dashboard (the apps' Open buttons): WKWebView shows
// nothing and returns nil without this method, so "apps are not opening in
// app window" — every Open click looked dead. This opens a real new window
// hosting a fresh WKWebView and reports it back so window.open returns a
// usable handle (the dashboard closes app windows through it on Stop).
- (WKWebView *)webView:(WKWebView *)webView createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
    forNavigationAction:(WKNavigationAction *)navigationAction windowFeatures:(WKWindowFeatures *)windowFeatures {
    NSWindow *pop = [[NSWindow alloc]
        initWithContentRect:NSMakeRect(0, 0, 1280, 880)
                  styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable
                          | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
                    backing:NSBackingStoreBuffered
                      defer:NO];
    WKWebView *wv = [[WKWebView alloc] initWithFrame:pop.contentView.bounds configuration:configuration];
    wv.UIDelegate = webView.UIDelegate;
    wv.navigationDelegate = webView.navigationDelegate;
    pop.contentView = wv;
    [pop center];
    [pop makeKeyAndOrderFront:nil];
    pop.releasedWhenClosed = NO;
    deckAddWindow(pop); // app windows follow the transparency knob too
    return wv;
}
// <input type="file"> / webkitdirectory picks: WKWebView shows no open
// panel without this delegate method, so the TokenReport "Pick folder…"
// fallback was dead in the native window. NSOpenPanel can choose folders
// (canChooseDirectories) which covers both file and directory picks.
- (void)webView:(WKWebView *)webView runOpenPanelWithParameters:(WKOpenPanelParameters *)parameters
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(NSArray<NSURL *> *URLs))completionHandler {
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.canChooseFiles = YES; // a single-file pick reports allowsMultipleSelection=NO — never map that onto canChooseFiles, or files are greyed out
    panel.canChooseDirectories = parameters.allowsDirectories;
    panel.allowsMultipleSelection = parameters.allowsMultipleSelection;
    [panel beginWithCompletionHandler:^(NSInteger result) {
        if (result == NSModalResponseOK) {
            completionHandler(panel.URLs);
        } else {
            completionHandler(nil);
        }
    }];
}
- (void)webView:(WKWebView *)webView runJavaScriptAlertPanelWithMessage:(NSString *)message
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(void))completionHandler {
    NSAlert *a = [NSAlert new];
    a.messageText = @"AppDeck";
    a.informativeText = message;
    [a addButtonWithTitle:@"OK"];
    [a runModal];
    completionHandler();
}
- (void)webView:(WKWebView *)webView runJavaScriptConfirmPanelWithMessage:(NSString *)message
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(BOOL))completionHandler {
    NSAlert *a = [NSAlert new];
    a.messageText = @"AppDeck";
    a.informativeText = message;
    [a addButtonWithTitle:@"OK"];
    [a addButtonWithTitle:@"Cancel"];
    completionHandler([a runModal] == NSAlertFirstButtonReturn);
}
// Navigation: allow everything, except a navigation that IS a download
// (a download-attribute anchor or an attachment response) — those become
// WKDownloads and flow through the WKDownloadDelegate methods below.
- (void)webView:(WKWebView *)webView decidePolicyForNavigationAction:(WKNavigationAction *)navigationAction
    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    if (navigationAction.shouldPerformDownload) {
        decisionHandler(WKNavigationActionPolicyDownload);
        return;
    }
    decisionHandler(WKNavigationActionPolicyAllow);
}
- (void)webView:(WKWebView *)webView decidePolicyForNavigationResponse:(WKNavigationResponse *)navigationResponse
    decisionHandler:(void (^)(WKNavigationResponsePolicy))decisionHandler {
    if (!navigationResponse.canShowMIMEType) { // Content-Disposition attachment, octet-stream…
        decisionHandler(WKNavigationResponsePolicyDownload);
        return;
    }
    decisionHandler(WKNavigationResponsePolicyAllow);
}
// WebKit turns a .download-policy navigation into a WKDownload that has NO
// delegate of its own: nobody is asked for a destination and the download is
// discarded a moment later (unified log: "Adding download N" then "Removing
// download N" ~200 ms after the click, no error callback). That is the
// "Download for macOS does nothing" report — RepoLens's button reached
// WebKit fine (policyAction=Download) and died right there. Handing the
// delegate over in BOTH hooks is what makes the destination callback below
// run; popups inherit this delegate through their navigationDelegate.
- (void)webView:(WKWebView *)webView navigationAction:(WKNavigationAction *)navigationAction
    didBecomeDownload:(WKDownload *)download {
    download.delegate = self;
    [self deckTrack:download inWebView:webView];
}
- (void)webView:(WKWebView *)webView navigationResponse:(WKNavigationResponse *)navigationResponse
    didBecomeDownload:(WKDownload *)download {
    download.delegate = self;
    [self deckTrack:download inWebView:webView];
}
// deckTrack registers the download for progress reporting and cancel.
- (void)deckTrack:(WKDownload *)download inWebView:(WKWebView *)webView {
    if (gDeckDownloads == nil) gDeckDownloads = [[NSMutableArray alloc] init];
    DeckDownloadItem *item = [[DeckDownloadItem new] autorelease];
    item.download = download;
    item.webView = webView;
    item.ident = ++gDeckDownloadSeq;
    [gDeckDownloads addObject:item];
    [download.progress addObserver:self forKeyPath:@"fractionCompleted"
                           options:NSKeyValueObservingOptionNew context:deckDownloadKVO];
    deckPushDownload(item, @"start", nil);
}
- (void)deckForget:(WKDownload *)download {
    DeckDownloadItem *item = deckItemForDownload(download);
    if (!item) return;
    [download.progress removeObserver:self forKeyPath:@"fractionCompleted" context:deckDownloadKVO];
    [gDeckDownloads removeObject:item];
}
- (void)observeValueForKeyPath:(NSString *)keyPath ofObject:(id)object change:(NSDictionary *)change
                      context:(void *)context {
    if (context != deckDownloadKVO) {
        [super observeValueForKeyPath:keyPath ofObject:object change:change context:context];
        return;
    }
    for (DeckDownloadItem *it in [gDeckDownloads copy]) {
        if (it.download.progress == object) {
            deckPushDownload(it, @"progress", nil);
            break;
        }
    }
}
// The download's one required decision: where it lands. "Ask each time" opens
// the OS save panel; otherwise the configured folder (or ~/Downloads) takes
// the server-suggested name, uniquified, no prompt — a browser's silent save.
- (void)download:(WKDownload *)download decideDestinationUsingResponse:(NSURLResponse *)response
    suggestedFilename:(NSString *)suggestedFilename
    completionHandler:(void (^)(NSURL *destination))completionHandler {
    DeckDownloadItem *item = deckItemForDownload(download);
    if (item && suggestedFilename.length) item.name = suggestedFilename;
    if (gDeckDownloadAsk) {
        NSSavePanel *panel = [NSSavePanel savePanel];
        panel.nameFieldStringValue = suggestedFilename.length ? suggestedFilename : @"download";
        NSModalResponse answer = [panel runModal];
        NSURL *picked = (answer == NSModalResponseOK) ? panel.URL : nil;
        if (item) item.destPath = picked.path;
        completionHandler(picked); // nil = the operator cancelled: no download
        return;
    }
    NSString *path = deckDownloadDestination(suggestedFilename);
    if (item) item.destPath = path;
    completionHandler(path ? [NSURL fileURLWithPath:path] : nil);
}
- (void)downloadDidFinish:(WKDownload *)download {
    DeckDownloadItem *item = deckItemForDownload(download);
    deckPushDownload(item, @"done", item.destPath.length
        ? [NSString stringWithFormat:@",path:'%@'", deckJSQuote(item.destPath)] : nil);
    [self deckForget:download];
    NSBeep(); // the one visible ack — the file is in the destination folder
}
- (void)download:(WKDownload *)download didFailWithError:(NSError *)error resumeData:(NSData *)resumeData {
    DeckDownloadItem *item = deckItemForDownload(download);
    NSLog(@"AppDeck: download failed: %@", error.localizedDescription);
    deckPushDownload(item, @"error", [NSString stringWithFormat:@",error:'%@'",
        deckJSQuote(error.localizedDescription ?: @"download failed")]);
    [self deckForget:download];
    NSBeep();
}
@end

// Window transparency: the dashboard's 🖼 dialog posts the knob (0..60)
// from JS as window.webkit.messageHandlers.appdeckTransparency.postMessage(n).
// NSWindow.alphaValue blends the whole window — content and title bar —
// toward whatever is behind it. The knob drives EVERY deck window: the
// dashboard itself plus the app windows opened from it (the popups below),
// so apps go see-through together with the deck. The initial value rides
// macRun's arg.
static NSMutableArray *gDeckWindows = nil; // dashboard + open app popups
static double gDeckPct = 0;                // last applied knob, 0..60
static double deckClampPct(double v) {
    if (v < 0) return 0;
    if (v > 100) return 100;
    return v;
}
static void deckApplyTransparency(void) {
    if (gDeckWindows == nil) return;
    for (NSWindow *w in gDeckWindows) {
        w.alphaValue = (CGFloat)(1.0 - gDeckPct / 100.0);
    }
}
static void deckAddWindow(NSWindow *w) {
    if (gDeckWindows == nil) gDeckWindows = [[NSMutableArray alloc] init];
    [gDeckWindows addObject:w]; // closed popups stay: pop.releasedWhenClosed = NO keeps them alive
    deckApplyTransparency();
}
@interface DeckTransparencyHandler : NSObject <WKScriptMessageHandler>
@end

@implementation DeckTransparencyHandler
- (void)userContentController:(WKUserContentController *)userContentController
        didReceiveScriptMessage:(WKScriptMessage *)message {
    double pct = deckClampPct([message.body doubleValue]);
    if (pct > 60) pct = 60; // the Go side caps the knob; clamped again here
    gDeckPct = pct;
    deckApplyTransparency();
}
@end

// DeckDownloadsHandler is the dashboard's handle on a running download:
// "reveal" shows the finished file in Finder, "cancel" stops the transfer.
@interface DeckDownloadsHandler : NSObject <WKScriptMessageHandler>
@end
@implementation DeckDownloadsHandler
- (void)userContentController:(WKUserContentController *)controller
        didReceiveScriptMessage:(WKScriptMessage *)message {
    if (![message.body isKindOfClass:[NSDictionary class]]) return;
    NSDictionary *body = message.body;
    NSString *action = body[@"action"];
    if ([action isEqualToString:@"reveal"]) {
        NSString *path = body[@"path"];
        if (path.length > 0) {
            [[NSWorkspace sharedWorkspace] activateFileViewerSelectingURLs:@[[NSURL fileURLWithPath:path]]];
        }
        return;
    }
    if ([action isEqualToString:@"cancel"]) {
        id ident = body[@"id"];
        DeckDownloadItem *item = deckItemForIdent([NSString stringWithFormat:@"%@", ident ? ident : @""]);
        if (item) [item.download cancel:^(NSData *resumeData) { (void)resumeData; }];
    }
}
@end

// Menu bar companion --------------------------------------------------------
// The tray gives the deck a persistent home in the menu bar: a glance at
// what is running, one-click launch (started apps open in a popup, exactly
// like the dashboard's Open buttons), a wTower pause/resume toggle, and the
// quit action. The menu rebuilds from Go's cached snapshot every time it
// opens; Go refreshes that snapshot on a 5 s ticker (tray.go), so opening
// the menu never probes anything.

// deckTrayIcon draws a small template rocket so the menu bar mark matches the
// AppDeck launcher icon. Template mode follows dark/light appearance.
static NSImage *deckTrayIcon(void) {
    NSImage *img = [[NSImage alloc] initWithSize:NSMakeSize(18, 18)];
    [img lockFocus];
    NSBezierPath *body = [NSBezierPath bezierPath];
    [body moveToPoint:NSMakePoint(9, 17)];
    [body curveToPoint:NSMakePoint(14, 8) controlPoint1:NSMakePoint(12.5, 15.5) controlPoint2:NSMakePoint(14, 11.5)];
    [body curveToPoint:NSMakePoint(9, 3) controlPoint1:NSMakePoint(14, 5) controlPoint2:NSMakePoint(11, 3)];
    [body curveToPoint:NSMakePoint(4, 8) controlPoint1:NSMakePoint(7, 3) controlPoint2:NSMakePoint(4, 5)];
    [body curveToPoint:NSMakePoint(9, 17) controlPoint1:NSMakePoint(4, 11.5) controlPoint2:NSMakePoint(5.5, 15.5)];
    NSBezierPath *fins = [NSBezierPath bezierPath];
    [fins moveToPoint:NSMakePoint(4.5, 8)];
    [fins lineToPoint:NSMakePoint(1.5, 12.5)];
    [fins lineToPoint:NSMakePoint(5, 11.5)];
    [fins moveToPoint:NSMakePoint(13.5, 8)];
    [fins lineToPoint:NSMakePoint(16.5, 12.5)];
    [fins lineToPoint:NSMakePoint(13, 11.5)];
    [fins moveToPoint:NSMakePoint(7, 3.5)];
    [fins lineToPoint:NSMakePoint(9, 0.8)];
    [fins lineToPoint:NSMakePoint(11, 3.5)];
    body.lineWidth = 1.35;
    fins.lineWidth = 1.35;
    [[NSColor blackColor] setStroke];
    [body stroke];
    [fins stroke];
    [[NSBezierPath bezierPathWithOvalInRect:NSMakeRect(7.5, 8.5, 3, 3)] stroke];
    [img unlockFocus];
    img.template = YES;
    return img;
}

// NOT static: tray_darwin.go links against this symbol.
void deckShowMainWindow(void) {
    if (!gDeckMainWindow) return;
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
    [gDeckMainWindow makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

// deckOpenAppURL opens an app URL in a popup via the main webview's
// window.open — the same path the dashboard's Open buttons take
// (createWebViewWithConfiguration). Callable from any thread: the AppKit
// work hops to the main queue, and the NSString copy outruns the C string.
// NOT static: tray_darwin.go links against this symbol.
void deckOpenAppURL(const char *url) {
    NSString *nsuml = url ? [NSString stringWithUTF8String:url] : nil;
    if (nsuml.length == 0) return;
    dispatch_async(dispatch_get_main_queue(), ^{
        if (!gDeckMainWebView) return;
        NSString *js = [NSString stringWithFormat:@"window.open('%@')", deckJSQuote(nsuml)];
        [gDeckMainWebView evaluateJavaScript:js completionHandler:nil];
    });
}

// DeckTrayDelegate rebuilds the menu on every open and owns the item
// actions. "Quit AppDeck" carries no target — terminate: lands in
// applicationShouldTerminate: (the Go bridge), like the menu bar item.
// DeckWindowHandler — page-initiated window controls. With the
// transparent titlebar the system's double-click-to-zoom is dead (the
// webview covers the titlebar views), so the dashboard double-clicks its
// glass header and this performs the standard zoom (fit the screen).
@interface DeckWindowHandler : NSObject <WKScriptMessageHandler>
@end
@implementation DeckWindowHandler
- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
    (void)userContentController;
    if (![message.name isEqualToString:@"appdeckWindow"]) return;
    NSDictionary *body = nil;
    if ([message.body isKindOfClass:[NSDictionary class]]) body = (NSDictionary *)message.body;
    NSString *action = body[@"action"] ?: @"";
    if ([action isEqualToString:@"zoom"] && gDeckMainWindow) {
        [gDeckMainWindow zoom:nil];
    }
}
@end

// trayModelFromGo parses the cached snapshot for the menu builders; nil
// when Go has nothing to hand over.
static NSDictionary *trayModelFromGo(void) {
    char *json = appdeckTrayMenuJSON();
    NSString *str = json ? [NSString stringWithUTF8String:json] : nil;
    if (json) free(json);
    NSData *data = [str dataUsingEncoding:NSUTF8StringEncoding];
    return data ? [NSJSONSerialization JSONObjectWithData:data options:0 error:nil] : nil;
}

@interface DeckTrayDelegate : NSObject <NSMenuDelegate>
@end
@implementation DeckTrayDelegate
- (void)menuNeedsUpdate:(NSMenu *)menu {
    if (menu != gDeckTrayMenu) {
        // A submenu (the Apps list): rebuild just its rows.
        [self rebuildAppRows:menu];
        return;
    }
    while (menu.numberOfItems > 0) [menu removeItemAtIndex:0];
    NSDictionary *model = trayModelFromGo();
    if (!model) {
        NSMenuItem *it = [menu addItemWithTitle:@"AppDeck" action:nil keyEquivalent:@""];
        it.enabled = NO;
        return;
    }
    long running = [model[@"running_count"] longLongValue];
    NSMenuItem *head = [menu addItemWithTitle:
        [NSString stringWithFormat:@"AppDeck — %ld app%@ running",
            running, running == 1 ? @"" : @"s"] action:nil keyEquivalent:@""];
    head.enabled = NO;

    NSMenuItem *search = [menu addItemWithTitle:@"Search apps…"
        action:@selector(deckTraySearch:) keyEquivalent:@""];
    search.target = self;

    NSMenuItem *dash = [menu addItemWithTitle:@"Open Dashboard"
        action:@selector(deckShowDashboard:) keyEquivalent:@""];
    dash.target = self;

    // Quick actions from the apps' manifests (trayToggle): "Pause <label>"
    // / "Resume <label>". The paused flag arrives as an NSNumber, which is
    // non-nil even when it wraps NO — always read boolValue, never test
    // the object (that once made this item say "Resume" forever).
    NSArray *toggles = model[@"toggles"] ?: @[];
    if (toggles.count) {
        [menu addItem:[NSMenuItem separatorItem]];
        for (NSDictionary *tg in toggles) {
            NSString *label = tg[@"label"] ?: @"";
            if (tg[@"err"]) {
                NSMenuItem *it = [menu addItemWithTitle:
                    [NSString stringWithFormat:@"%@ — %@", label, tg[@"err"]]
                    action:nil keyEquivalent:@""];
                it.enabled = NO;
                continue;
            }
            BOOL paused = [tg[@"paused"] boolValue];
            NSMenuItem *it = [menu addItemWithTitle:
                [NSString stringWithFormat:@"%@ %@", paused ? @"Resume" : @"Pause", label]
                action:@selector(deckTrayToggle:) keyEquivalent:@""];
            it.target = self;
            it.representedObject = @{@"id": tg[@"id"] ?: @"", @"paused": @(paused)};
        }
    }

    // Apps ▸ bounded submenu — the top-level menu stays compact no matter
    // how many apps are installed; the submenu fills on open.
    NSMenuItem *appsItem = [menu addItemWithTitle:@"Apps" action:nil keyEquivalent:@""];
    NSMenu *sub = [[NSMenu new] autorelease];
    sub.delegate = self; // menuNeedsUpdate routes submenus to rebuildAppRows:
    appsItem.submenu = sub;

    [menu addItem:[NSMenuItem separatorItem]];
    [menu addItemWithTitle:@"Quit AppDeck" action:@selector(terminate:) keyEquivalent:@""];
}

// rebuildAppRows fills the Apps submenu: the ◐ filter row, then running
// apps (or the whole deck when the filter is on), capped so a 50-app deck
// cannot fill the screen. Called when the submenu opens and again live
// after the filter toggles.
- (void)rebuildAppRows:(NSMenu *)sub {
    while (sub.numberOfItems > 0) [sub removeItemAtIndex:0];
    NSDictionary *model = trayModelFromGo();
    if (!model) return;
    BOOL runningOnly = [model[@"running_only"] boolValue];
    NSMenuItem *filter = [sub addItemWithTitle:@"Show Running Only"
        action:@selector(deckTrayRunningOnly:) keyEquivalent:@""];
    filter.target = self;
    filter.state = runningOnly ? NSControlStateValueOn : NSControlStateValueOff;
    [sub addItem:[NSMenuItem separatorItem]];

    NSArray *apps = model[@"apps"] ?: @[];
    NSMutableArray *pool = [NSMutableArray new];
    for (NSDictionary *a in apps) {
        if ([a[@"running"] boolValue]) [pool addObject:a]; // running first:
    }                                                       // the cap keeps the useful set
    if (runningOnly) {
        for (NSDictionary *a in apps) {
            if (![a[@"running"] boolValue]) [pool addObject:a];
        }
    }
    const NSInteger maxRows = 10;
    for (NSInteger i = 0; i < (NSInteger)pool.count && i < maxRows; i++) {
        NSDictionary *a = pool[i];
        NSString *name = a[@"name"] ?: (a[@"id"] ?: @"?");
        NSString *mark = [a[@"running"] boolValue] ? @"● " : @"○ ";
        NSMenuItem *row = [sub addItemWithTitle:[mark stringByAppendingString:name]
            action:@selector(deckTrayApp:) keyEquivalent:@""];
        row.target = self;
        row.representedObject = a;
    }
    if ((NSInteger)pool.count > maxRows) {
        NSMenuItem *more = [sub addItemWithTitle:
            [NSString stringWithFormat:@"+%ld more…", (long)(pool.count - maxRows)]
            action:@selector(deckTraySearch:) keyEquivalent:@""];
        more.target = self;
    } else if (pool.count == 0) {
        NSMenuItem *none = [sub addItemWithTitle:@"No apps running"
            action:nil keyEquivalent:@""];
        none.enabled = NO;
    }
}

- (void)deckShowDashboard:(id)sender {
    (void)sender;
    deckShowMainWindow();
}
- (void)deckTraySearch:(id)sender {
    (void)sender;
    appdeckTraySearch();
}
- (void)deckTrayToggle:(NSMenuItem *)sender {
    NSDictionary *d = sender.representedObject;
    NSString *appID = d[@"id"] ?: @"";
    BOOL paused = [d[@"paused"] boolValue];
    // paused → resume, and back; Go owns the POST and re-probes afterwards.
    appdeckTrayToggle(deckTrayStrDup(appID.UTF8String), paused ? 0 : 1);
}
- (void)deckTrayApp:(NSMenuItem *)sender {
    NSDictionary *d = sender.representedObject;
    NSString *appID = d[@"id"] ?: @"";
    NSString *url = d[@"url"] ?: @"";
    appdeckTrayAppAction(deckTrayStrDup(appID.UTF8String),
                         deckTrayStrDup(url.UTF8String),
                         [d[@"running"] boolValue] ? 1 : 0);
}
- (void)deckTrayRunningOnly:(NSMenuItem *)sender {
    // The export flips Go's flag and returns the new state, so the open
    // submenu re-filters immediately; Go's refresh lands for the next open.
    int next = appdeckTrayRunningOnlyToggle();
    sender.state = next ? NSControlStateValueOn : NSControlStateValueOff;
    [self rebuildAppRows:sender.menu];
}
@end

// deckTrayStrDup copies a string for the //export bridge — the Go side
// owns (and frees) the copy. strdup's declaration rides <string.h> above.
static char *deckTrayStrDup(const char *s) {
    size_t n = strlen(s ? s : "") + 1;
    char *p = (char *)malloc(n);
    if (p) memcpy(p, s ? s : "", n);
    return p;
}

// deckTrayInstall builds the status item and flips the keep-running
// contract. The activity claim stops App Nap from suspending the process
// once the (hidden) window stops drawing. Any click opens the menu via
// .menu — the contract the icon always had. (A button-action split —
// left-click dashboard, right-click menu — was tried and reverted: plain
// clicks stopped delivering the action on current macOS, and only
// ctrl-click reached the handler. Windows keeps that split; it is its
// pre-existing contract.)
static void deckTrayInstall(void) {
    gDeckKeepRunning = 1;
    [[NSProcessInfo processInfo] beginActivityWithOptions:NSActivityUserInitiated
                                                   reason:@"AppDeck menu bar companion"];
    gDeckTrayDelegate = [DeckTrayDelegate new];
    NSMenu *m = [NSMenu new]; // held by gDeckTrayMenu for the process's life
    m.delegate = gDeckTrayDelegate;
    gDeckTrayMenu = m;
    gDeckTray = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
    gDeckTray.button.image = deckTrayIcon();
    gDeckTray.menu = m;
}

// deckTraySetVisible shows/hides the status item live — the General
// settings "Menu bar companion" toggle. NOT static: tray_darwin.go's cgo
// call links against this symbol. Turning it off also restores the
// close-to-quit contract (gDeckKeepRunning), exactly like a launcher
// booted without the companion; turning it on (re)installs.
void deckTraySetVisible(int on) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (on) {
            if (!gDeckTray) deckTrayInstall();
            gDeckTray.visible = YES;
        } else if (gDeckTray) {
            gDeckTray.visible = NO;
        }
        gDeckKeepRunning = on ? 1 : 0;
    });
}

// macRun opens url in a native WKWebView window and blocks until the window
// closes or the app quits. Returns 1 when the UI ran (caller shuts the
// launcher down), 0 when it could not be created (caller falls back to the
// browser app window). transp is the persisted window-transparency knob
// (0..60 percent), applied at creation; later changes arrive through the
// script message handler above. tray installs the menu bar companion and
// switches the close button to hide-instead-of-quit.
static int macRun(const char *curl, int transp, int tray) {
    @autoreleasepool {
        NSString *nsuml = [NSString stringWithUTF8String:curl];
        NSURL *url = nsuml ? [NSURL URLWithString:nsuml] : nil;
        if (url == nil) return 0;

        NSApplication *app = [NSApplication sharedApplication];
        MacRunDelegate *del = [MacRunDelegate new];
        app.delegate = del; // alive for the whole [app run] below
        // LSUIElement keeps the bare controller out of the Dock at launch;
        // Regular restores the full app presence while the window is open.
        [app setActivationPolicy:NSApplicationActivationPolicyRegular];

        // Minimal menu bar: the Quit item (terminate: lands in
        // applicationShouldTerminate: above) plus the standard Edit actions
        // so copy/paste/select-all work inside the dashboard's inputs.
        NSMenu *menu = [[NSMenu new] autorelease];
        NSMenuItem *appItem = [[NSMenuItem new] autorelease];
        [menu addItem:appItem];
        NSMenu *appMenu = [[NSMenu new] autorelease];
        [appMenu addItem:[[[NSMenuItem alloc] initWithTitle:@"Quit AppDeck"
            action:@selector(terminate:) keyEquivalent:@"q"] autorelease]];
        [appItem setSubmenu:appMenu];
        NSMenuItem *editItem = [[NSMenuItem new] autorelease];
        [menu addItem:editItem];
        NSMenu *editMenu = [[NSMenu new] autorelease];
        [editMenu addItem:[[[NSMenuItem alloc] initWithTitle:@"Cut"
            action:@selector(cut:) keyEquivalent:@"x"] autorelease]];
        [editMenu addItem:[[[NSMenuItem alloc] initWithTitle:@"Copy"
            action:@selector(copy:) keyEquivalent:@"c"] autorelease]];
        [editMenu addItem:[[[NSMenuItem alloc] initWithTitle:@"Paste"
            action:@selector(paste:) keyEquivalent:@"v"] autorelease]];
        [editMenu addItem:[[[NSMenuItem alloc] initWithTitle:@"Select All"
            action:@selector(selectAll:) keyEquivalent:@"a"] autorelease]];
        [editItem setSubmenu:editMenu];
        [app setMainMenu:menu];

        // Prism-style window: the titlebar is transparent and the content
        // runs full-height, so the dashboard's glass reaches the very top
        // and the traffic lights float over it (the dashboard's mac-frame
        // layout clears the lights and the drag strip). The window itself
        // keeps closing/minimizing/zooming through the real lights.
        NSWindowStyleMask style = NSWindowStyleMaskTitled
            | NSWindowStyleMaskClosable
            | NSWindowStyleMaskMiniaturizable
            | NSWindowStyleMaskResizable
            | NSWindowStyleMaskFullSizeContentView;
        NSWindow *win = [[NSWindow alloc]
            initWithContentRect:NSMakeRect(0, 0, 1280, 830)
                      styleMask:style
                        backing:NSBackingStoreBuffered
                          defer:NO];
        [win setTitle:@"AppDeck"];
        win.titlebarAppearsTransparent = YES;
        win.titleVisibility = NSWindowTitleHidden;
        [win setMovableByWindowBackground:YES];
        [win center];
        // Tray builds: the red close button hides the window instead of
        // closing the app (windowShouldClose: below), so the window object
        // must survive the close, and the delegate must see the close.
        win.releasedWhenClosed = NO;
        win.delegate = del;
        gDeckMainWindow = win;

        // The transparency bridge rides the webview's user content
        // controller; popup windows (createWebViewWithConfiguration)
        // inherit this configuration, so the knob keeps working with app
        // windows open.
        gDeckPct = deckClampPct((double)transp);
        if (gDeckPct > 60) gDeckPct = 60;
        deckAddWindow(win);

        WKWebViewConfiguration *cfg = [WKWebViewConfiguration new];
        [cfg.userContentController addScriptMessageHandler:[[DeckTransparencyHandler new] autorelease]
                                                       name:@"appdeckTransparency"];
        // Window controls from the page: with the transparent titlebar the
        // system double-click-to-zoom is covered by the webview, so the
        // page sends a zoom request (double-click on the glass header).
        [cfg.userContentController addScriptMessageHandler:[[DeckWindowHandler new] autorelease]
                                                       name:@"appdeckWindow"];
        // Download strip actions (reveal in Finder / cancel a transfer).
        [cfg.userContentController addScriptMessageHandler:[[DeckDownloadsHandler new] autorelease]
                                                       name:@"appdeckDownload"];
        WKWebView *wv = [[WKWebView alloc] initWithFrame:win.contentView.bounds configuration:cfg];
        MacJSDelegate *jsd = [[MacJSDelegate new] autorelease];
        wv.UIDelegate = jsd;
        // The same object carries WKDownloadDelegate — the webview routes
        // .download-policy navigations to its navigation delegate, so one
        // delegate covers panels, picks and downloads.
        wv.navigationDelegate = jsd;
        win.contentView = wv;
        gDeckMainWebView = wv;

        if (tray) deckTrayInstall();

        [win makeKeyAndOrderFront:nil];
        [app activateIgnoringOtherApps:YES];
        [wv loadRequest:[NSURLRequest requestWithURL:url]];

        [app run]; // returns when the loop is stopped from any quit path
        return 1;
    }
}
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// Pin the main goroutine to the process's main thread before anything can
// block and let the scheduler migrate it: AppKit tolerates UI calls only on
// thread 0, and a launch that first probes ports/loads apps hands the main
// goroutine to another OS thread (reproduced as an
// NSInternalInconsistencyException from setMainMenu on a non-main thread).
// Package init runs on the main goroutine before main(), so this is early
// enough. The HTTP server and all apps keep running on other threads.
func init() { runtime.LockOSThread() }

// runNativeWindow opens url in the launcher's own window and blocks until the
// window is closed or quit. Mirrors the Windows WebView2 contract in
// native_windows.go. dataPath is the Windows WebView2 profile directory;
// WKWebView keeps its website data in the per-app default store, so it is
// unused here. trayRequested installs the menu bar companion.
func runNativeWindow(url, dataPath string) bool {
	_ = dataPath
	tray := 0
	if trayRequested {
		tray = 1
	}
	return C.macRun(C.CString(url), C.int(initialWindowTransparency), C.int(tray)) != 0
}

// nativeConfigureDownloads pushes the download destination into the window's
// download delegate: a fixed folder, or "ask" mode where every download opens
// the OS save panel. dir == "" means ~/Downloads. Called at boot and after
// every settings change, so a new destination applies to the next click.
func nativeConfigureDownloads(dir string, ask bool) {
	cDir := C.CString(dir)
	defer C.free(unsafe.Pointer(cDir))
	a := C.int(0)
	if ask {
		a = 1
	}
	C.deckSetDownloadDir(cDir)
	C.deckSetDownloadAsk(a)
}
