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

// JS panel delegate: WKWebView suppresses window.confirm/alert unless a
// UIDelegate answers them. Without this, the dashboard's Quit confirmation
// silently returned false and the button appeared dead.
@interface MacJSDelegate : NSObject <WKUIDelegate>
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
        WKWebView *wv = [[WKWebView alloc] initWithFrame:win.contentView.bounds configuration:cfg];
        wv.UIDelegate = [[MacJSDelegate new] autorelease];
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

import "runtime"

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
