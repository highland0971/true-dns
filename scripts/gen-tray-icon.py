#!/usr/bin/env python3
"""Generates cmd/truedns/assets/tray.ico — a 32x32 32bpp BMP-style ICO icon
(blue circle with a white center dot, the true-dns DNS-dot motif).

Usage: python3 scripts/gen-tray-icon.py
"""
import math
import os
import struct

S = 32


def build() -> bytes:
    cx = cy = 15.5
    r_outer = 14.5
    r_inner = 4.5
    # top-down RGBA rows
    rows = []
    for y in range(S):
        row = []
        for x in range(S):
            d = math.hypot(x - cx, y - cy)
            if d <= r_outer:
                px = (255, 255, 255, 255) if d <= r_inner else (37, 99, 235, 255)
            else:
                px = (0, 0, 0, 0)
            row.append(px)
        rows.append(row)

    # XOR data: BGRA, bottom-up rows
    xor = b""
    for y in range(S - 1, -1, -1):
        for x in range(S):
            r, g, b, a = rows[y][x]
            xor += bytes((b, g, r, a))
    and_mask = bytes(S * S // 8)  # fully opaque

    bih = struct.pack("<IiiHHIIiiII", 40, S, S * 2, 1, 32, 0, 0, 0, 0, 0, 0)
    body = bih + xor + and_mask
    entry = struct.pack("<BBBBHHII", S, S, 0, 0, 1, 32, len(body), 6 + 16)
    header = struct.pack("<HHH", 0, 1, 1)
    return header + entry + body


def main() -> None:
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    out_dir = os.path.join(root, "cmd", "truedns", "assets")
    os.makedirs(out_dir, exist_ok=True)
    out = os.path.join(out_dir, "tray.ico")
    with open(out, "wb") as f:
        f.write(build())
    print(f"wrote {out} ({os.path.getsize(out)} bytes)")


if __name__ == "__main__":
    main()
