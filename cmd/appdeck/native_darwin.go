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

// Provided by native_darwin_quit.go (//export appdeckRequestQuit):
// hands every AppKit quit path straight to the Go shutdown.
extern void appdeckRequestQuit(void);

// Forward declaration: the popup path below (createWebViewWithConfiguration)
// registers app windows with the transparency tracker defined further down.
static void deckAddWindow(NSWindow *w);


// MacRunDelegate owns the window lifecycle. Every quit path — ⌘Q, the Quit
// menu item, and the red close button — is routed through
// applicationShouldTerminate:, which CANCELS AppKit's terminate (a plain
// terminate would exit() the process from Objective-C and skip the Go
// shutdown that stops the launched apps) and instead stops the NSApp run
// loop. [app run] then returns to Go, which runs the same beginShutdown
// contract as window close on Windows: kill the apps, then exit.
@interface MacRunDelegate : NSObject <NSApplicationDelegate>
@property (assign) BOOL shuttingDown;
@end

@implementation MacRunDelegate
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)app {
    return YES; // route the red close button through applicationShouldTerminate:
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

// macRun opens url in a native WKWebView window and blocks until the window
// closes or the app quits. Returns 1 when the UI ran (caller shuts the
// launcher down), 0 when it could not be created (caller falls back to the
// browser app window). transp is the persisted window-transparency knob
// (0..60 percent), applied at creation; later changes arrive through the
// script message handler above.
static int macRun(const char *curl, int transp) {
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

        NSWindowStyleMask style = NSWindowStyleMaskTitled
            | NSWindowStyleMaskClosable
            | NSWindowStyleMaskMiniaturizable
            | NSWindowStyleMaskResizable;
        NSWindow *win = [[NSWindow alloc]
            initWithContentRect:NSMakeRect(0, 0, 1280, 830)
                      styleMask:style
                        backing:NSBackingStoreBuffered
                          defer:NO];
        [win setTitle:@"AppDeck"];
        [win center];

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
// unused here.
func runNativeWindow(url, dataPath string) bool {
	_ = dataPath
	return C.macRun(C.CString(url), C.int(initialWindowTransparency)) != 0
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
