#!/usr/bin/env python3
"""Render Launcher/icon.ico — a flat rocket on the dashboard's blue-cyan
gradient rounded square (matches the 🚀 dashboard logo).

Usage:  python tools/makeicon.py          (writes icon.ico + icon-preview.png)
Requires Pillow.
"""
import math
import os

from PIL import Image, ImageDraw

S = 2048  # supersampled render size
OUT = os.path.join(os.path.dirname(__file__), "..", "icon.ico")
PREVIEW = os.path.join(os.path.dirname(__file__), "..", "icon-preview.png")

ACCENT = (0x6D, 0x8D, 0xFF)   # dashboard --accent
ACCENT2 = (0x38, 0xC6, 0xF4)  # dashboard --accent-2
BODY = (0xF8, 0xFA, 0xFC)
BODY_SHADE = (0xD9, 0xE2, 0xEF)
FIN = (0xFF, 0x5C, 0x6C)      # dashboard --red, softened
FLAME = (0xFF, 0xB0, 0x20)    # dashboard --amber
FLAME_IN = (0xFF, 0xD1, 0x66)
WINDOW = (0x2F, 0x6F, 0xD8)
WINDOW_RING = (0x1B, 0x4E, 0x9E)


def rounded_mask(size, radius):
    m = Image.new("L", (size, size), 0)
    d = ImageDraw.Draw(m)
    d.rounded_rectangle([0, 0, size - 1, size - 1], radius=radius, fill=255)
    return m


def diagonal_gradient(size, c1, c2):
    small = 256
    grad = Image.new("RGB", (small, small))
    px = grad.load()
    for y in range(small):
        for x in range(small):
            t = (x + y) / (2 * (small - 1))
            px[x, y] = tuple(int(c1[i] + (c2[i] - c1[i]) * t) for i in range(3))
    return grad.resize((size, size), Image.BICUBIC)


def rocket(draw, s):
    """Flat rocket, nose up, centered on a size-s canvas."""
    cx = s / 2
    w = s * 0.30          # body width
    y0 = s * 0.16         # nose tip
    y1 = s * 0.66         # body bottom
    flame_len = s * 0.20
    steps = 64

    def body_outline():
        """Bullet body: eased nose curve into straight sides, flat base."""
        nose_h = s * 0.22
        pts = []
        for i in range(steps + 1):
            t = i / steps
            ease = math.sin(math.pi / 2 * t)
            pts.append((cx - w / 2 * ease, y0 + nose_h * t))
        pts.append((cx - w / 2, y1))
        pts.append((cx + w / 2, y1))
        for i in range(steps, -1, -1):
            t = i / steps
            ease = math.sin(math.pi / 2 * t)
            pts.append((cx + w / 2 * ease, y0 + nose_h * t))
        return pts

    def teardrop(top_w, tip_y, color):
        pts = []
        h = tip_y - y1
        for i in range(steps + 1):
            t = i / steps
            ease = math.sin(math.pi / 2 * (1 - t))  # wide at top → point
            pts.append((cx - top_w / 2 * ease, y1 + h * t))
        for i in range(steps, -1, -1):
            t = i / steps
            ease = math.sin(math.pi / 2 * (1 - t))
            pts.append((cx + top_w / 2 * ease, y1 + h * t))
        draw.polygon(pts, fill=color)

    # flame (behind body)
    teardrop(w * 0.52, y1 + flame_len, FLAME)
    teardrop(w * 0.28, y1 + flame_len * 0.62, FLAME_IN)

    # side fins (behind body, attached to the body sides, swept out and down)
    fin_h = s * 0.19
    fy0 = y1 - s * 0.115
    for side in (-1, 1):
        draw.polygon([
            (cx + side * (w / 2 - s * 0.004), fy0),
            (cx + side * (w / 2 + s * 0.085), fy0 + fin_h * 0.72),
            (cx + side * (w / 2 + s * 0.082), fy0 + fin_h),
            (cx + side * (w / 2 - s * 0.006), fy0 + fin_h * 0.55),
        ], fill=FIN)

    # body
    draw.polygon(body_outline(), fill=BODY)

    # porthole window
    wy = y0 + s * 0.30
    r = w * 0.27
    draw.ellipse([cx - r, wy - r, cx + r, wy + r],
                 fill=WINDOW, outline=WINDOW_RING, width=int(s * 0.012))
    # small glint
    gr = r * 0.28
    gx, gy = cx - r * 0.35, wy - r * 0.38
    draw.ellipse([gx - gr, gy - gr, gx + gr, gy + gr], fill=(210, 234, 255))


def main():
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    gradient = diagonal_gradient(S, ACCENT, ACCENT2).convert("RGBA")
    img.paste(gradient, (0, 0), rounded_mask(S, int(S * 0.22)))
    rocket(ImageDraw.Draw(img), S)

    master = img.resize((512, 512), Image.LANCZOS)
    master.save(PREVIEW)
    ico = master.resize((256, 256), Image.LANCZOS)
    ico.save(OUT, sizes=[(16, 16), (24, 24), (32, 32), (48, 48),
                         (64, 64), (128, 128), (256, 256)])
    print("wrote", os.path.abspath(OUT), "and", os.path.abspath(PREVIEW))


if __name__ == "__main__":
    main()
