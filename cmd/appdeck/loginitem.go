// Launch-at-login: register the launcher with the operating system so it
// starts when the user logs in. Each OS file implements setLoginItem; the
// enabled flag itself lives in launcher.json (the launcher-settings store)
// and the registration is re-asserted on boot, so an updated or moved
// executable rewrites the OS record with the new path.
package main

// setLoginItem registers (enabled) or removes the launcher's login item,
// exePath being the absolute path of the running executable.
//
// macOS: a LaunchAgent plist — ~/Library/LaunchAgents/com.appdeck.launcher
// (.plist), RunAtLoad so launchd starts it at login. Takes effect next login
// (or after `launchctl kickstart`); no launchd load is attempted from here.
//
// Windows: the per-user HKCU Run key — no elevation, survives updates, and
// the standard place users look to remove it.
//
// Linux: an XDG autostart .desktop entry in ~/.config/autostart, which GNOME,
// KDE and the other mainstream desktops all honor.
