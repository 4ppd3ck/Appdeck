#!/bin/sh
# Assemble AppDeck releases.
#
#   ./release.sh                        small launcher, all default targets
#   ./release.sh windows/amd64          one target
#   ./release.sh --all-in-one           the legacy everything-inside build
#   ./release.sh --all-in-one windows/amd64
#
# DEFAULT RELEASE — the launcher alone. No apps inside: customers install
# apps separately (Import, or drop a folder with app.json into Apps/). The
# exe is a few MB. Apps ship as their own folders/exes.
#
# --all-in-one (optional, for customers who want zero install): the legacy
# single-file build — every app from Apps/ is packed inside one executable
# (app binaries, web assets, dashboard, registry) and unpacked to the user's
# data directory at runtime. The app set is DISCOVERED, not maintained here:
# tools/genembedded scans the project root (every folder with a go.mod or an
# HTML entry page, except the launcher's own checkout, whatever it is named)
# — deleting an app folder and re-running removes it from the exe; adding a
# folder adds it.
#
# Output in release/:
#   appdeck-<os>-<arch>[.exe]           small launcher (default mode)
#   appdeck-allinone-<os>-<arch>[.exe]  single-file build (--all-in-one)
#   + platform packaging: Windows zip / macOS .app / Linux .desktop bundle
#   SHA256SUMS
set -e
cd "$(dirname "$0")"
PROJECT_ROOT="$(cd .. && pwd)"
OUT="$(pwd)/release"
EMB="$(pwd)/embedded"
mkdir -p "$OUT"
# Sweep this tool's own artifacts from earlier runs so stale files cannot
# ride along in the new SHA256SUMS.
rm -rf "$OUT"/appdeck-* "$OUT"/AppDeck-* "$OUT"/SHA256SUMS  # -rf: backups of previously installed bundles are directories

if ! command -v go >/dev/null 2>&1; then
    echo "error: Go toolchain (go 1.27+) is required" >&2
    exit 1
fi

ALL_IN_ONE=0
TARGETS=""
for arg in "$@"; do
    case "$arg" in
        --all-in-one) ALL_IN_ONE=1 ;;
        *) TARGETS="$TARGETS $arg" ;;
    esac
done
TARGETS=${TARGETS:-"windows/amd64 windows/arm64 windows/386 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64"}

# The host platform: desktop apps built for it get a native cgo build so
# their GUI ships; every other target is cross-compiled CGO_ENABLED=0.
HOSTOS=$(go env GOOS)
HOSTARCH=$(go env GOARCH)
ERRFILE="$(mktemp)"

LDFLAGS="-s -w -X main.launcherVersion=$(date +%Y-%m-%d)"

# copy_assets <src-dir> <dest-dir> — runtime files only (html/css/js/data/…).
copy_assets() {
    src=$1; dst=$2
    mkdir -p "$dst"
    tar -C "$src" \
        --exclude='*.go' --exclude='go.mod' --exclude='go.sum' \
        --exclude='start.bat' --exclude='start.sh' --exclude='start.cmd' \
        --exclude='run.sh' --exclude='build.sh' \
        --exclude='README.md' \
        --exclude='*.db' --exclude='*.db-shm' --exclude='*.db-wal' \
        --exclude='*.exe' --exclude='*.log' \
        --exclude='dist' --exclude='logs' --exclude='cmd' --exclude='data_raw' \
        --exclude='.git' --exclude='node_modules' \
        --exclude='release' --exclude='tmp' \
        --exclude='state.json' --exclude='state.json.tmp' \
        --exclude='.DS_Store' --exclude='Thumbs.db' \
        --exclude='.env' --exclude='config.env' \
        --exclude='data/cache' \
        --exclude='server.js' --exclude='package.json' --exclude='lib' \
        --exclude='webserver' \
        --exclude='__pycache__' --exclude='*.pyc' \
        --exclude='.github' --exclude='.gitignore' --exclude='.gitattributes' \
        --exclude='docs' --exclude='icons' --exclude='internal' \
        --exclude='packaging' --exclude='*.syso' \
        --exclude='CHANGELOG.md' --exclude='LICENSE' --exclude='gitadd.md' \
        -cf - . | tar -C "$dst" -xf -
}

# go build helper: build <os> <arch> <goarm> <dir> <entrypoint> <outfile>
build() {
    local os=$1 arch=$2 goarm=$3 dir=$4 entry=$5 bout=$6
    (cd "$PROJECT_ROOT/$dir" && \
        GOOS=$os GOARCH=$arch GOARM=${goarm:-} CGO_ENABLED=0 \
        go build -trimpath -ldflags "$LDFLAGS" -o "$bout" "$entry")
}

# build_app <os> <arch> <goarm> <dir> <entrypoint> <outfile> <desktop> — a
# desktop app built for the host platform gets a native cgo build so its GUI
# ships (Gio's darwin/linux backends link the window system and cannot cross-
# compile); everywhere else — and on any cgo failure — the plain
# CGO_ENABLED=0 build runs. A CLI-only desktop binary still starts; it
# prints its own explanation when the launcher tries to open its GUI.
build_app() {
    local os=$1 arch=$2 goarm=$3 dir=$4 entry=$5 bout=$6 desktop=$7
    if [ "$desktop" = "1" ] && [ "$os" = "$HOSTOS" ] && [ "$arch" = "$HOSTARCH" ]; then
        if (cd "$PROJECT_ROOT/$dir" && \
                GOOS=$os GOARCH=$arch GOARM=${goarm:-} CGO_ENABLED=1 \
                go build -trimpath -ldflags "$LDFLAGS" -o "$bout" "$entry" 2>"$ERRFILE"); then
            return 0
        fi
        echo "   note: native cgo GUI build failed — shipping a CLI-only build ($(tail -n 1 "$ERRFILE" | cut -c1-160))"
    fi
    build "$os" "$arch" "$goarm" "$dir" "$entry" "$bout"
}

restore_embedded() {
    rm -rf embedded
    mkdir -p embedded
    printf 'Replaced by release.sh at build time.\n' > embedded/.placeholder
}

# archive <stage-dir> <archive-name-without-ext> <entries...> — zip when
# available (macOS, most Linuxes), tar.gz otherwise. The tar.gz fallback is
# deliberate for these bundles: they carry unix executables that must keep
# the exec bit after extraction, and the one zip writer a Windows host has
# (PowerShell's Compress-Archive) stores no unix permissions — a
# Compress-Archive'd .app would extract non-executable. tar keeps the bit.
archive() {
    stage=$1; name=$2; shift 2
    if command -v zip >/dev/null 2>&1; then
        (cd "$stage" && zip -r -q "$OUT/$name.zip" "$@")
        echo "   + $name.zip"
    else
        (cd "$stage" && tar -czf "$OUT/$name.tar.gz" "$@")
        echo "   + $name.tar.gz"
    fi
}

# zip_windows <file> <name-without-ext> — the Windows distribution zip: the
# bare exe at the archive root. zip is used where present; Git Bash (the
# host release.cmd drives) ships no zip.exe, so PowerShell's Compress-Archive
# is the fallback — fine for a Windows artifact, since no unix exec bits need
# preserving. Unlike archive() there is deliberately no tar.gz fallback: the
# .zip name is part of the release contract, so a host with neither writer
# must fail loudly.
zip_windows() {
    file=$1; name=$2
    if command -v zip >/dev/null 2>&1; then
        (cd "$OUT" && zip -q "$name.zip" "$(basename "$file")")
    elif command -v powershell.exe >/dev/null 2>&1; then
        # Paths via env vars (no shell quoting reaches PowerShell); cygpath
        # converts /c/... to C:\... (always present in Git Bash).
        ZIP_SRC="$(cygpath -w "$file" 2>/dev/null || printf '%s' "$file")" \
        ZIP_DST="$(cygpath -w "$OUT/$name.zip" 2>/dev/null || printf '%s' "$OUT/$name.zip")" \
        powershell.exe -NoProfile -Command 'Compress-Archive -Path $env:ZIP_SRC -DestinationPath $env:ZIP_DST -Force'
    else
        echo "error: cannot write $name.zip — install zip (e.g. apt install zip) or run on Windows with PowerShell" >&2
        exit 1
    fi
    echo "   + $name.zip"
}

# zip_stage <stage-dir> <name-without-ext> — zip a release folder's contents
# (folder-mode small releases: exe + index.html + apps.json + Apps/).
zip_stage() {
    stage=$1; name=$2
    if command -v zip >/dev/null 2>&1; then
        (cd "$stage" && zip -q -r "$OUT/$name.zip" .)
    elif command -v powershell.exe >/dev/null 2>&1; then
        ZIP_STAGE="$(cygpath -w "$stage")" \
        ZIP_DST="$(cygpath -w "$OUT/$name.zip")" \
        powershell.exe -NoProfile -Command 'Compress-Archive -Path (Join-Path $env:ZIP_STAGE "*") -DestinationPath $env:ZIP_DST -Force'
    else
        echo "error: cannot write $name.zip — install zip or run on Windows with PowerShell" >&2
        exit 1
    fi
    echo "   + $name.zip"
}
# proper .app (Info.plist + AppDeck.icns + ad-hoc signature). Without the
# bundle Finder treats the bare executable as an unknown document ("Siri is
# unable to open…") and it can show no icon; with it, AppDeck drags into
# /Applications and double-clicks like any Mac app.
make_app_bundle() {
    arch=$1 bin=$2 rtdir=$3
    stage="$(mktemp -d)"
    app="$stage/AppDeck.app"
    mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
    cp "$bin" "$app/Contents/MacOS/AppDeck"
    chmod +x "$app/Contents/MacOS/AppDeck"
    # Folder-mode releases carry the dashboard + registry scaffold inside the
    # bundle (the launcher resolves its root from the exe's folder).
    if [ -n "$rtdir" ]; then
        cp "$rtdir"/index.html "$rtdir"/apps.json "$app/Contents/MacOS/"
        mkdir -p "$app/Contents/MacOS/Apps"
    fi
    if [ -f AppDeck.icns ]; then
        cp AppDeck.icns "$app/Contents/Resources/AppDeck.icns"
    fi
    cat > "$app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key><string>AppDeck</string>
    <key>CFBundleDisplayName</key><string>AppDeck</string>
    <key>CFBundleExecutable</key><string>AppDeck</string>
    <key>CFBundleIdentifier</key><string>com.manny.appdeck</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>1.1</string>
    <key>CFBundleVersion</key><string>1.1</string>
    <key>CFBundleIconFile</key><string>AppDeck</string>
    <key>NSHighResolutionCapable</key><true/>
    <key>LSMinimumSystemVersion</key><string>10.13</string>
    <!-- Agent app: AppDeck's visible face is the dashboard app window it
         opens (Brave/Edge/Chrome --app). Without this the bare controller
         bounces in the Dock at launch and then just sits there windowless. -->
    <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
    if command -v codesign >/dev/null 2>&1; then
        codesign --force --sign - "$app" >/dev/null 2>&1 || true
    fi
    archive "$stage" "AppDeck-darwin-$arch" AppDeck.app
    rm -rf "$stage"
}

# make_linux_bundle <arch> <bare-binary> — ELF executables carry no icon;
# ship the XDG desktop entry + icon beside the binary so Linux desktops list
# AppDeck in application menus with its icon.
make_linux_bundle() {
    arch=$1 bin=$2 rtdir=$3
    stage="$(mktemp -d)"
    cp "$bin" "$stage/appdeck"
    chmod +x "$stage/appdeck"
    cp appdeck-icon.png "$stage/appdeck.png"
    if [ -n "$rtdir" ]; then
        cp "$rtdir"/index.html "$rtdir"/apps.json "$stage/"
        mkdir -p "$stage/Apps"
    fi
    cat > "$stage/appdeck.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=AppDeck
Comment=One page for every local app — start, stop, open
Exec=appdeck
Icon=appdeck
Terminal=false
Categories=Utility;
DESKTOP
    cat > "$stage/INSTALL.txt" <<'TXT'
AppDeck — Linux install
1. Copy "appdeck"         to ~/.local/bin            (create the folder if needed)
2. Copy "appdeck.png"     to ~/.local/share/icons/
3. Copy "appdeck.desktop" to ~/.local/share/applications/
Log out and back in (or run `update-desktop-database ~/.local/share/applications`)
and AppDeck appears in your applications menu with its icon.
TXT
    archive "$stage" "AppDeck-linux-$arch" appdeck appdeck.png appdeck.desktop INSTALL.txt index.html apps.json Apps
    rm -rf "$stage"
}

restore_embedded
trap 'restore_embedded; rm -f "$ERRFILE"' EXIT

for target in $TARGETS; do
    os=${target%%/*}
    arch=${target##*/}
    goarm=""
    case "$arch" in arm) goarm=7 ;; esac
    exe=""; [ "$os" = "windows" ] && exe=".exe"
    if [ "$ALL_IN_ONE" = "1" ]; then
        out="appdeck-allinone-$os-$arch$exe"
        pkgname="appdeck-allinone-$os-$arch"
    else
        out="appdeck-$os-$arch$exe"
        pkgname="appdeck-$os-$arch"
    fi

    echo "=== $os/$arch ==="

    if [ "$ALL_IN_ONE" = "1" ]; then
        # ---- the legacy single-file build: every app packed inside --------
        rm -rf "$EMB"
        mkdir -p "$EMB/bin" "$EMB/assets"
        go run ./tools/genembedded -root "$PROJECT_ROOT" -apps apps.json \
            -out "$EMB/apps.json" -manifest "$EMB/manifest.tsv"

        while IFS="$(printf '\t')" read -r kind id folder entry binname assets desktop; do
            [ -n "$kind" ] || continue
            if [ "$kind" = "static" ]; then
                echo "-- assets: $id (hosted in launcher)"
                copy_assets "$PROJECT_ROOT/$folder" "$EMB/assets/$id"
            elif [ "$assets" = "1" ]; then
                echo "-- $binname (binary + assets)"
                build_app "$os" "$arch" "$goarm" "$folder" "$entry" "$EMB/bin/$binname" "$desktop"
                copy_assets "$PROJECT_ROOT/$folder" "$EMB/assets/$id"
                if [ "$desktop" = "1" ] && [ -f "$EMB/bin/$binname" ]; then
                    mkdir -p "$EMB/assets/$id/data"
                    cp "$EMB/bin/$binname" "$EMB/assets/$id/data/"
                fi
            else
                echo "-- $binname (binary only — assets are embedded in the app)"
                build_app "$os" "$arch" "$goarm" "$folder" "$entry" "$EMB/bin/$binname" "$desktop"
                for ic in icon.png icon.svg; do
                    if [ -f "$PROJECT_ROOT/$folder/$ic" ]; then
                        mkdir -p "$EMB/assets/$id"
                        cp "$PROJECT_ROOT/$folder/$ic" "$EMB/assets/$id/$ic"
                    fi
                done
            fi
        done < "$EMB/manifest.tsv"
    fi

    # ---- the launcher executable -------------------------------------------
    echo "-- $out"
    ldflags="$LDFLAGS"
    [ "$os" = "windows" ] && ldflags="-H windowsgui $LDFLAGS"
    TAGS="gui"
    [ "$ALL_IN_ONE" = "1" ] && TAGS="gui embed"
    # A darwin build for the host arch links WKWebView via cgo
    # (native_darwin.go) so macOS gets the native window; every other target
    # stays CGO_ENABLED=0 (pure cross-compile). On any cgo failure the
    # non-cgo build runs and the app window / browser fallback ships.
    launcher_cgo=0
    if [ "$os" = "darwin" ] && [ "$os" = "$HOSTOS" ] && [ "$arch" = "$HOSTARCH" ]; then
        launcher_cgo=1
    fi
    if ! (GOOS=$os GOARCH=$arch GOARM=${goarm:-} CGO_ENABLED=$launcher_cgo \
            go build -tags "$TAGS" -trimpath -ldflags "$ldflags" -o "$OUT/$out" . 2>"$ERRFILE"); then
        echo "   note: cgo launcher build failed — shipping browser-window fallback ($(tail -n 1 "$ERRFILE" | cut -c1-160))"
        (GOOS=$os GOARCH=$arch GOARM=${goarm:-} CGO_ENABLED=0 \
            go build -tags "$TAGS" -trimpath -ldflags "$ldflags" -o "$OUT/$out" .)
    fi

    # Ad-hoc code signature for macOS (only meaningful when run on macOS).
    if [ "$os" = "darwin" ] && command -v codesign >/dev/null 2>&1; then
        codesign --force -s - "$OUT/$out" 2>/dev/null || true
    fi

    # Folder-mode releases carry the dashboard + an empty registry beside the
    # exe: without index.html the launcher answers {"error":"index.html
    # missing"} on every page. All-in-one builds carry everything inside.
    RTDIR=""
    if [ "$ALL_IN_ONE" != "1" ]; then
        RTDIR="$(mktemp -d)"
        cp index.html "$RTDIR/"
        printf '{"launcher_port": 9090, "apps": []}\n' > "$RTDIR/apps.json"
        mkdir -p "$RTDIR/Apps"
        cp "$OUT/$out" "$RTDIR/appdeck$exe"
    fi

    # Packaging: Windows distribution zip / macOS .app bundle / Linux
    # .desktop + icon.
    case "$os" in
        windows)
            if [ -n "$RTDIR" ]; then
                zip_stage "$RTDIR" "$pkgname"
            else
                zip_windows "$OUT/$out" "$pkgname"
            fi ;;
        darwin)  make_app_bundle "$arch" "$OUT/$out" "$RTDIR" ;;
        linux)   make_linux_bundle "$arch" "$OUT/$out" "$RTDIR" ;;
    esac
    [ -n "$RTDIR" ] && rm -rf "$RTDIR"

    restore_embedded

    ls -lh "$OUT/$out" | awk '{printf "   %s  (%s)\n", $9, $5}'
done

echo
echo "== checksums =="
( cd "$OUT" && for f in appdeck-* AppDeck-*; do [ -f "$f" ] && sha256sum "$f"; done ) > "$OUT/SHA256SUMS" || true
if ! [ -s "$OUT/SHA256SUMS" ]; then
    ( cd "$OUT" && for f in appdeck-* AppDeck-*; do [ -f "$f" ] && shasum -a 256 "$f"; done ) > "$OUT/SHA256SUMS"
fi
cat "$OUT/SHA256SUMS"
echo
if [ "$ALL_IN_ONE" = "1" ]; then
    echo "done. Ship appdeck-allinone-<os>-<arch>[.exe] — one file, everything inside."
else
    echo "done. Small launcher shipped — apps install separately (Import, or drop app.json folders into Apps/)."
    echo "Need the everything-inside build for a customer? ./release.sh --all-in-one"
fi
