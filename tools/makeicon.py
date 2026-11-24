#!/usr/bin/env python3
"""Generate AppDeck's outlined-rocket launcher icons.

Usage: python tools/makeicon.py
Requires Pillow; macOS sips is used to write the ICNS variant.
"""
import os
import shutil
import struct
import subprocess
import tempfile
from io import BytesIO

from PIL import Image, ImageDraw

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
S = 2048
LANCZOS = getattr(getattr(Image, "Resampling", Image), "LANCZOS")
ICON_SIZES = (16, 24, 32, 48, 64, 128, 256)
SCALE = 1.6
OFFSET = (64 - 24 * SCALE) / 2
TILE = (0x1B, 0x1B, 0x38, 255)
EDGE = (0x48, 0x45, 0x78, 255)
ROCKET = (0x9A, 0x91, 0xFF, 255)
WINDOW_LANGUAGE = 1033
WINDOW_MACHINES = {
    "386": 0x014C,
    "amd64": 0x8664,
    "arm64": 0xAA64,
}


def cubic(start, control1, control2, end, steps=32):
    points = []
    for i in range(steps + 1):
        t = i / steps
        u = 1 - t
        points.append((
            u**3 * start[0] + 3 * u**2 * t * control1[0]
            + 3 * u * t**2 * control2[0] + t**3 * end[0],
            u**3 * start[1] + 3 * u**2 * t * control1[1]
            + 3 * u * t**2 * control2[1] + t**3 * end[1],
        ))
    return points


def render_icon():
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)
    draw.rounded_rectangle(
        (0, 0, S - 1, S - 1),
        radius=round(S * 0.235),
        fill=TILE,
        outline=EDGE,
        width=round(S / 64),
    )

    unit = S / 64
    stroke = round(1.15 * SCALE * unit)

    def transform(point):
        return (
            round((point[0] * SCALE + OFFSET) * unit),
            round((point[1] * SCALE + OFFSET) * unit),
        )

    def line(points, closed=False):
        mapped = [transform(point) for point in points]
        if closed:
            mapped.append(mapped[0])
        draw.line(mapped, fill=ROCKET, width=stroke, joint="curve")

    # Tail spark.
    spark = [(4.6, 16.6)]
    spark += cubic((4.6, 16.6), (3.2, 17.8), (2.5, 21.4), (2.5, 21.4))
    spark += cubic((2.5, 21.4), (2.5, 21.4), (6.1, 20.7), (7.3, 19.3))[1:]
    spark += cubic((7.3, 19.3), (8.3, 18.3), (5.6, 15.7), (4.6, 16.6))[1:]
    line(spark)

    # Rocket body and its swept fins.
    body = [(12, 15), (9, 12)]
    body += cubic((9, 12), (10.6, 8.2), (13.7, 4.8), (21, 3))[1:]
    body += cubic((21, 3), (20.4, 10.3), (17, 13.4), (13.2, 15))[1:]
    body += [(12, 15)]
    line(body)
    line([(9, 12), (4.5, 12)] + cubic(
        (4.5, 12), (4.5, 12), (5.1, 8.7), (8.6, 7.5))[1:])
    line([(12, 15), (12, 19.5)] + cubic(
        (12, 19.5), (12, 19.5), (15.3, 18.9), (16.5, 15.4))[1:])

    center = transform((16.5, 7.5))
    radius = round(1 * SCALE * unit)
    draw.ellipse(
        (center[0] - radius, center[1] - radius,
         center[0] + radius, center[1] + radius),
        outline=ROCKET,
        width=stroke,
    )
    return img


def png_bytes(master, size):
    image = master.resize((size, size), LANCZOS)
    out = BytesIO()
    image.save(out, format="PNG", optimize=True)
    return out.getvalue()


def resource_directory(entries):
    header = struct.pack("<IIHHHH", 0, 0, 0, 0, 0, len(entries))
    return header + b"".join(struct.pack("<II", name, target) for name, target in entries)


def windows_resource_object(machine, icon_payloads):
    count = len(icon_payloads)
    icon_type_offset = 32
    icon_languages_offset = icon_type_offset + 16 + count * 8
    group_type_offset = icon_languages_offset + count * 24
    group_language_offset = group_type_offset + 24
    data_entries_offset = group_language_offset + 24
    group_entry_offset = data_entries_offset + count * 16
    payload_offset = data_entries_offset + (count + 1) * 16
    payload_offset = (payload_offset + 3) & ~3

    resource = bytearray(payload_offset)
    root = resource_directory([
        (3, icon_type_offset | 0x80000000),
        (14, group_type_offset | 0x80000000),
    ])
    resource[0:len(root)] = root

    icon_entries = [
        (index + 2, (icon_languages_offset + index * 24) | 0x80000000)
        for index in range(count)
    ]
    icon_directory = resource_directory(icon_entries)
    resource[icon_type_offset:icon_type_offset + len(icon_directory)] = icon_directory

    group_type = resource_directory([(1, group_language_offset | 0x80000000)])
    resource[group_type_offset:group_type_offset + len(group_type)] = group_type
    group_language = resource_directory([
        (WINDOW_LANGUAGE, group_entry_offset),
    ])
    resource[group_language_offset:group_language_offset + len(group_language)] = group_language

    group = struct.pack("<HHH", 0, 1, count)
    payloads = []
    next_payload = payload_offset
    for index, (size, payload) in enumerate(icon_payloads):
        lang_offset = icon_languages_offset + index * 24
        data_offset = data_entries_offset + index * 16
        lang = resource_directory([(WINDOW_LANGUAGE, data_offset)])
        resource[lang_offset:lang_offset + len(lang)] = lang
        resource[data_offset:data_offset + 16] = struct.pack(
            "<IIII", next_payload, len(payload), 0, 0)
        payloads.append((next_payload, payload))
        dimension = 0 if size == 256 else size
        icon_id = index + 2
        group += struct.pack(
            "<BBBBHHIH", dimension, dimension, 0, 0, 1, 32, len(payload), icon_id)
        next_payload = (next_payload + len(payload) + 3) & ~3

    resource[group_entry_offset:group_entry_offset + 16] = struct.pack(
        "<IIII", next_payload, len(group), 0, 0)
    payloads.append((next_payload, group))
    next_payload += len(group)

    resource.extend(b"\0" * (next_payload - len(resource)))
    for offset, payload in payloads:
        resource[offset:offset + len(payload)] = payload

    section_size = (len(resource) + 3) & ~3
    resource.extend(b"\0" * (section_size - len(resource)))
    coff_header = struct.pack("<HHIIIHH", machine, 1, 0, 0, 0, 0, 0)
    section_header = struct.pack(
        "<8sIIIIIIHHI",
        b".rsrc\0\0\0", len(resource), 0, len(resource), 60, 0, 0, 0, 0,
        0x40000040,
    )
    return coff_header + section_header + resource


def write_windows_resources(master):
    payloads = [(size, png_bytes(master, size)) for size in ICON_SIZES]
    for arch, machine in WINDOW_MACHINES.items():
        path = os.path.join(ROOT, "cmd", "appdeck", f"rsrc_windows_{arch}.syso")
        with open(path, "wb") as f:
            f.write(windows_resource_object(machine, payloads))


def write_macos_icon(master):
    iconutil = shutil.which("iconutil")
    destination = os.path.join(ROOT, "cmd", "appdeck", "AppDeck.icns")
    if iconutil:
        iconset_names = {
            "icon_16x16.png": 16,
            "icon_16x16@2x.png": 32,
            "icon_32x32.png": 32,
            "icon_32x32@2x.png": 64,
            "icon_128x128.png": 128,
            "icon_128x128@2x.png": 256,
            "icon_256x256.png": 256,
            "icon_256x256@2x.png": 512,
        }
        with tempfile.TemporaryDirectory() as temp:
            iconset = os.path.join(temp, "AppDeck.iconset")
            os.mkdir(iconset)
            for name, size in iconset_names.items():
                master.resize((size, size), LANCZOS).save(
                    os.path.join(iconset, name))
            output = os.path.join(temp, "AppDeck.icns")
            subprocess.run(
                [iconutil, "-c", "icns", iconset, "-o", output],
                check=True,
                stdout=subprocess.DEVNULL,
            )
            os.replace(output, destination)
        return True

    sips = shutil.which("sips")
    if sips:
        with tempfile.TemporaryDirectory(dir=os.path.dirname(destination)) as temp:
            output = os.path.join(temp, "AppDeck.icns")
            subprocess.run(
                [sips, "-s", "format", "icns", os.path.join(ROOT, "appdeck-icon.png"),
                 "--out", output],
                check=True,
                stdout=subprocess.DEVNULL,
            )
            os.replace(output, destination)
        return True
    print("note: no macOS icon converter found; AppDeck.icns was not regenerated")
    return False


def main():
    master = render_icon()
    preview = os.path.join(ROOT, "icon-preview.png")
    icon_png = os.path.join(ROOT, "appdeck-icon.png")
    icon_ico = os.path.join(ROOT, "icon.ico")
    master.resize((512, 512), LANCZOS).save(preview)
    master.resize((256, 256), LANCZOS).save(icon_png)
    master.resize((256, 256), LANCZOS).save(
        icon_ico, sizes=[(size, size) for size in ICON_SIZES])
    write_windows_resources(master)
    write_macos_icon(master)
    print("wrote appdeck-icon.png, icon.ico, icon-preview.png, AppDeck.icns, and Windows resources")


if __name__ == "__main__":
    main()
