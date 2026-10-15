#!/bin/sh
# Build every app for the HOST platform with the binary names the launcher
# expects: <name>-<goos>-<goarch>[.exe] next to each app's go.mod (see
# platformBinaryPath in main.go).
#
#   ./build.sh                    build all apps + the launcher
#   ./build.sh --app <name>       build ONE app's exe into its own folder
#                                 (dev loop: change code → build → restart the
#                                 app from the deck). <name> matches the app's
#                                 id, folder name or binary name, e.g.
#                                 ./build.sh --app promptpress
#   ./build.sh --app=<name>       same, = form
#   ./build.sh --console          also emit appdeck-console.exe (dev build)
#
# The app set is DISCOVERED (tools/genembedded scans the project root) —
# adding or removing an app folder needs no edits here.
#
# The launcher itself is built twice on Windows:
#   appdeck.exe            GUI build (-tags gui, no console window) — the one
#                          to double-click for daily use
#   appdeck-console.exe    normal console build for development
#
# Cross-platform release zips for clients are built by release.sh instead.
set -e
cd "$(dirname "$0")"
ROOT="$(cd .. && pwd)"

APP_NAME=""
CONSOLE=0
while [ $# -gt 0 ]; do
    case "$1" in
        --console) CONSOLE=1 ;;
        --app) [ $# -ge 2 ] || { echo "error: --app needs a name" >&2; exit 1; }
               APP_NAME="$2"; shift ;;
        --app=*) APP_NAME="${1#--app=}" ;;
        *) echo "error: unknown option '$1' (supported: --app <name>, --app=<name>, --console)" >&2
           exit 1 ;;
    esac
    shift
done

if ! command -v go >/dev/null 2>&1; then
    echo "error: Go toolchain (go 1.27+) is required" >&2
    exit 1
fi

GOOS=${GOOS:-$(go env GOOS)}
GOARCH=${GOARCH:-$(go env GOARCH)}
SUFFIX="-$GOOS-$GOARCH"
EXE=""
[ "$GOOS" = "windows" ] && EXE=".exe"
VERFLAG="-X main.launcherVersion=$(date +%Y-%m-%d)"
export CGO_ENABLED=0

echo "== host build: $GOOS/$GOARCH =="

MANIFEST="$(mktemp)"
ERRFILE="$(mktemp)"

# build <dir> <entrypoint> <binary-name> <desktop>
build() {
    local dir=$1 entry=$2 name=$3 desktop=$4
    echo "-- $name$SUFFIX$EXE"
    if [ "$desktop" = "1" ]; then
        # Desktop GUI apps need cgo on darwin/linux — their Gio backends link
        # the OS window system (on Windows the backend is pure Go, so cgo
        # changes nothing there). If the C toolchain or GUI dev headers are
        # missing, fall back to a CLI-only build and say so; such a binary
        # prints its own explanation when the launcher starts it GUI-style.
        if (cd "$ROOT/$dir" && CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" \
                -o "$name$SUFFIX$EXE" "$entry" 2>"$ERRFILE"); then
            return 0
        fi
        echo "   note: cgo GUI build failed — shipping a CLI-only build ($(tail -n 1 "$ERRFILE" | cut -c1-160))"
    fi
    (cd "$ROOT/$dir" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$name$SUFFIX$EXE" "$entry")
}

# The apps: discovered from the project root via the shared generator.
go run ./tools/genembedded -root "$ROOT" -apps apps.json \
    -out "$(mktemp)" -manifest "$MANIFEST" >/dev/null

if [ -n "$APP_NAME" ]; then
    # Single-app dev build: match on id, folder name or binary name
    # (case-insensitive), build just that exe, skip everything else.
    WANT=$(printf '%s' "$APP_NAME" | tr '[:upper:]' '[:lower:]')
    FOUND=0
    CANDIDATES=""
    while IFS="$(printf '\t')" read -r kind id folder entry binname assets desktop; do
        CANDIDATES="$CANDIDATES  $id ($kind)\n"
        LID=$(printf '%s' "$id" | tr '[:upper:]' '[:lower:]')
        LNAME=$(printf '%s' "$binname" | tr '[:upper:]' '[:lower:]')
        LFOLDER=$(printf '%s' "$folder" | tr '[:upper:]' '[:lower:]')
        LFOLDER=${LFOLDER##*/}
        if [ "$WANT" = "$LID" ] || [ "$WANT" = "$LNAME" ] || [ "$WANT" = "$LFOLDER" ]; then
            if [ "$kind" != "go" ]; then
                rm -f "$MANIFEST" "$ERRFILE"
                echo "error: '$id' is a hosted HTML app — it has no binary to build" >&2
                exit 1
            fi
            build "$folder" "$entry" "$binname" "$desktop"
            FOUND=1
            break
        fi
    done < "$MANIFEST"
    rm -f "$MANIFEST" "$ERRFILE"
    if [ "$FOUND" != "1" ]; then
        echo "error: no app matches '$APP_NAME'. Buildable apps:" >&2
        printf '%b' "$CANDIDATES" >&2
        exit 1
    fi
    echo
    echo "done. Built into the app's own folder — restart it from the deck to pick up the new exe."
    exit 0
fi

while IFS="$(printf '\t')" read -r kind id folder entry binname assets desktop; do
    [ "$kind" = "go" ] || continue
    build "$folder" "$entry" "$binname" "$desktop"
done < "$MANIFEST"

# Hosted web apps: standalone server exe beside the files, so each runs
# without the deck — double-click serves the folder and opens the browser.
while IFS="$(printf '\t')" read -r kind id folder entry binname assets desktop; do
    [ "$kind" = "static" ] || continue
    echo "-- webserver: $id"
    (go build -trimpath -ldflags "-s -w" -o "$ROOT/$folder/webserver$EXE" ./tools/webapp) \
        || echo "   note: webserver build failed for $id"
done < "$MANIFEST"
rm -f "$MANIFEST" "$ERRFILE"

# Launcher: GUI build (window mode; no console on Windows) is the primary one.
echo "-- appdeck (gui)$EXE"
if [ "$GOOS" = "windows" ]; then
    (go build -tags gui -trimpath -ldflags "-H windowsgui -s -w" -o "appdeck$EXE" .)
elif [ "$GOOS" = "darwin" ]; then
    # The darwin launcher links WKWebView via cgo (native_darwin.go) so the
    # dashboard opens in a real native window instead of a browser window;
    # without a C toolchain it falls back to the browser app window.
    if ! (CGO_ENABLED=1 go build -tags gui -trimpath -ldflags "-s -w $VERFLAG" -o appdeck . 2>"$ERRFILE"); then
        echo "   note: cgo launcher build failed — shipping browser-window fallback ($(tail -n 1 "$ERRFILE" | cut -c1-160))"
        (CGO_ENABLED=0 go build -tags gui -trimpath -ldflags "-s -w $VERFLAG" -o appdeck .)
    fi
else
    (go build -tags gui -trimpath -ldflags "-s -w $VERFLAG" -o appdeck .)
fi

if [ "$CONSOLE" = "1" ]; then
    echo "-- appdeck (console)$EXE"
    (go build -trimpath -ldflags "$VERFLAG" -o "appdeck-console$EXE" .)
fi

echo
echo "done. Start with: ./appdeck$EXE  →  http://127.0.0.1:9090"
