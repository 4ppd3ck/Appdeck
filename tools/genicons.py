#!/usr/bin/env python3
"""genicons — generate per-app icon.png files from the apps.json emoji.

Each app folder gets an icon.png (emoji centered on the AppDeck gradient,
rounded corners) next to the app's own files. The launcher's /api/icon
endpoint serves it to the dashboard; the emoji in apps.json stays as the
fallback when no icon.png exists. Running this is never required — drop any
icon.png or icon.svg into an app folder by hand and it wins.

Usage (from Launcher/):
    python3 tools/genicons.py             # create icons that are missing
    python3 tools/genicons.py --force     # regenerate ALL icons

Requires Pillow; color-emoji rendering uses macOS's Apple Color Emoji font
(on other systems the gradient tile is still produced; pass --no-emoji to
skip the emoji layer there). The gradient is sampled from icon-preview.png:
#5D9DFB → #47B5F7.
"""

import argparse
import json
import os

from PIL import Image, ImageDraw, ImageFont

SS = 4                        # supersample factor for smooth corners
SIZE = 256                    # final icon size
RAD = int(SIZE * 0.225) * SS  # macOS-style corner radius
TOP, BOT = (93, 157, 251), (71, 181, 247)
FONT_CANDIDATES = [
    "/System/Library/Fonts/Apple Color Emoji.ttc",   # macOS
    "/System/Library/Fonts/Segoe UI Emoji.ttf",      # sometimes present via ports
]


def tile_gradient():
    """4x supersampled rounded diagonal gradient tile, downscaled to SIZE."""
    big = Image.new("RGB", (SIZE * SS, SIZE * SS))
    d = ImageDraw.Draw(big)
    for y in range(SIZE * SS):
        t = y / (SIZE * SS - 1)
        row = tuple(round(a + (b - a) * t) for a, b in zip(TOP, BOT))
        d.line([(0, y), (SIZE * SS, y)], fill=row)
    mask = Image.new("L", (SIZE * SS, SIZE * SS), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [0, 0, SIZE * SS - 1, SIZE * SS - 1], radius=RAD, fill=255)
    out = Image.new("RGBA", (SIZE * SS, SIZE * SS), (0, 0, 0, 0))
    out.paste(big, (0, 0), mask)
    return out.resize((SIZE, SIZE), Image.LANCZOS)


def load_emoji_font():
    for path in FONT_CANDIDATES:
        if os.path.exists(path):
            try:
                return ImageFont.truetype(path, 160)
            except OSError:
                continue
    return None


def emoji_layer(font, ch):
    """Transparent layer with the emoji ink-centered; None if it renders empty."""
    layer = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    ImageDraw.Draw(layer).text((SIZE / 2, SIZE / 2), ch, font=font,
                               embedded_color=True, anchor="mm")
    bbox = layer.getbbox()
    if bbox is None:
        return None
    cx, cy = (bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2
    shifted = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    ImageDraw.Draw(shifted).text(
        (SIZE - cx, SIZE - cy), ch, font=font, embedded_color=True, anchor="mm")
    return shifted


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    launcher = os.path.dirname(here)
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--apps", default=os.path.join(launcher, "apps.json"))
    ap.add_argument("--force", action="store_true",
                    help="overwrite existing icon.png files")
    ap.add_argument("--no-emoji", action="store_true",
                    help="gradient tile only (no emoji layer)")
    args = ap.parse_args()

    with open(args.apps, encoding="utf-8") as f:
        cfg = json.load(f)
    font = None if args.no_emoji else load_emoji_font()

    made = skipped = 0
    for app in cfg["apps"]:
        rel = app.get("dir", "").replace("\\", "/")
        for prefix in ("../", "./"):
            rel = rel.removeprefix(prefix)
        folder = os.path.normpath(os.path.join(launcher, "..", rel))
        icon_path = os.path.join(folder, "icon.png")
        if os.path.exists(icon_path) and not args.force:
            skipped += 1
            continue
        tile = tile_gradient()
        if font and app.get("icon"):
            em = emoji_layer(font, app["icon"])
            if em:
                tile.alpha_composite(em)
            else:
                print(f"  note: emoji {app['icon']!r} rendered empty for "
                      f"{app['name']}; gradient-only tile written")
        tile.save(icon_path)
        made += 1
        print(f"icon.png -> {folder}  ({app['icon']} {app['name']})")
    print(f"\n{made} written, {skipped} already present (use --force to "
          f"regenerate)")


if __name__ == "__main__":
    main()
