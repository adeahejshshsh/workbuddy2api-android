#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把一张正方形源图缩成安卓五档 mipmap 图标（纯标准库，不需要 PIL/ImageMagick）。

    python3 patches/make_icons.py docs/WorkBuddy.png apk-src/res

* 解码：支持 8/16 位、色彩类型 0/2/3/4/6、非交错（源图 800x800 RGBA 实测走这条）
* 缩放：**面积平均**（每个目标像素按覆盖面积加权平均源像素），在预乘 alpha 空间做，
  避免半透明边缘发黑；不是最近邻那种锯齿货
* 输出：mipmap-{mdpi 48,hdpi 72,xhdpi 96,xxhdpi 144,xxxhdpi 192}/ic_launcher.png

为什么自己写：环境里没有 PIL（Pillow 要现场编译）也没有 ImageMagick，
而图标缩放的活儿一次性的、逻辑又短，自己写比拉个依赖干净。
"""
import os
import struct
import sys
import zlib

TARGETS = [("mipmap-mdpi", 48), ("mipmap-hdpi", 72), ("mipmap-xhdpi", 96),
           ("mipmap-xxhdpi", 144), ("mipmap-xxxhdpi", 192)]


def decode_png(path):
    """→ (w, h, rgba bytearray)。够用就行：非交错、无调色板还原以外的花活。"""
    b = open(path, "rb").read()
    if b[:8] != b"\x89PNG\r\n\x1a\n":
        raise SystemExit("不是 PNG: " + path)
    pos, idat, plte, trns = 8, bytearray(), None, None
    w = h = bd = ct = il = None
    while pos < len(b):
        ln = struct.unpack(">I", b[pos:pos + 4])[0]
        typ = b[pos + 4:pos + 8]
        data = b[pos + 8:pos + 8 + ln]
        if typ == b"IHDR":
            w, h, bd, ct, _cm, _fl, il = struct.unpack(">IIBBBBB", data)
        elif typ == b"IDAT":
            idat += data
        elif typ == b"PLTE":
            plte = data
        elif typ == b"tRNS":
            trns = data
        elif typ == b"IEND":
            break
        pos += 12 + ln
    if il:
        raise SystemExit("不支持交错 PNG")
    if bd not in (8, 16):
        raise SystemExit("暂不支持位深 %d" % bd)
    ch = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[ct]
    bpp = ch * (bd // 8)
    raw = zlib.decompress(bytes(idat))
    stride = w * bpp
    out = bytearray(stride * h)
    prev = bytearray(stride)
    p = 0
    for y in range(h):
        f = raw[p]; p += 1
        line = bytearray(raw[p:p + stride]); p += stride
        if f == 1:
            for i in range(bpp, stride):
                line[i] = (line[i] + line[i - bpp]) & 0xFF
        elif f == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif f == 3:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif f == 4:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                bb = prev[i]
                c = prev[i - bpp] if i >= bpp else 0
                pa, pb, pc = abs(bb - c), abs(a - c), abs(a + bb - 2 * c)
                pr = a if (pa <= pb and pa <= pc) else (bb if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        elif f != 0:
            raise SystemExit("未知过滤类型 %d" % f)
        out[y * stride:(y + 1) * stride] = line
        prev = line

    step = bd // 8
    if bd == 16:                                  # PNG 是大端，偶数下标即高字节 → 8 位近似
        out = bytearray(b for i, b in enumerate(out) if i % 2 == 0)
        stride = w * ch
        bpp = ch

    def px(x, y):
        o = y * stride + x * bpp
        if ct == 6:
            r, g, bl, a = out[o], out[o + 1], out[o + 2], out[o + 3]
        elif ct == 2:
            r, g, bl, a = out[o], out[o + 1], out[o + 2], 255
        elif ct == 0:
            r = g = bl = out[o]; a = 255
        elif ct == 4:
            r = g = bl = out[o]; a = out[o + 1]
        else:                                     # 调色板
            i = out[o]
            r, g, bl = plte[i * 3], plte[i * 3 + 1], plte[i * 3 + 2]
            a = trns[i] if (trns and i < len(trns)) else 255
        return r, g, bl, a

    rgba = bytearray(w * h * 4)
    for y in range(h):
        for x in range(w):
            r, g, bl, a = px(x, y)
            o = (y * w + x) * 4
            rgba[o:o + 4] = bytes((r, g, bl, a))
    return w, h, rgba


def resize_box(w, h, rgba, tw, th):
    """面积平均缩小（放大没需求，做最近邻兜底）。"""
    out = bytearray(tw * th * 4)
    sx, sy = w / tw, h / th
    for j in range(th):
        y0, y1 = j * sy, (j + 1) * sy
        iy0, iy1 = int(y0), min(h, int(y1 - 1e-9) + 1)
        for i in range(tw):
            x0, x1 = i * sx, (i + 1) * sx
            ix0, ix1 = int(x0), min(w, int(x1 - 1e-9) + 1)
            ar = ag = ab = aa = wt = 0.0
            for yy in range(iy0, iy1):
                wy = min(y1, yy + 1) - max(y0, yy)
                if wy <= 0:
                    continue
                row = yy * w
                for xx in range(ix0, ix1):
                    wx = min(x1, xx + 1) - max(x0, xx)
                    if wx <= 0:
                        continue
                    q = (row + xx) * 4
                    a = rgba[q + 3]
                    m = wx * wy * (a / 255.0)       # 预乘权重，边缘不发黑
                    ar += rgba[q] * m
                    ag += rgba[q + 1] * m
                    ab += rgba[q + 2] * m
                    aa += a * wx * wy
                    wt += wx * wy
            o = (j * tw + i) * 4
            if wt <= 0:
                continue
            alpha = aa / wt
            if alpha < 0.5:                         # 全透明像素
                out[o:o + 4] = b"\x00\x00\x00\x00"
                continue
            k = 255.0 / aa                          # 反预乘
            out[o] = min(255, int(ar * k + 0.5))
            out[o + 1] = min(255, int(ag * k + 0.5))
            out[o + 2] = min(255, int(ab * k + 0.5))
            out[o + 3] = min(255, int(alpha + 0.5))
    return out


def encode_png(w, h, rgba):
    raw = bytearray()
    stride = w * 4
    for y in range(h):
        raw.append(0)                               # 过滤器 None
        raw += rgba[y * stride:(y + 1) * stride]

    def chunk(typ, data):
        return (struct.pack(">I", len(data)) + typ + data +
                struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
            + chunk(b"IEND", b""))


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else "docs/WorkBuddy.png"
    res = sys.argv[2] if len(sys.argv) > 2 else "apk-src/res"
    w, h, rgba = decode_png(src)
    print(f"源图 {src}: {w}x{h}")
    if w != h:
        print("  警告：不是正方形，按最小边中心裁切")
        s = min(w, h)
        ox, oy = (w - s) // 2, (h - s) // 2
        crop = bytearray(s * s * 4)
        for y in range(s):
            crop[y * s * 4:(y + 1) * s * 4] = rgba[((y + oy) * w + ox) * 4:((y + oy) * w + ox + s) * 4]
        w = h = s
        rgba = crop
    for d, size in TARGETS:
        px = resize_box(w, h, rgba, size, size)
        png = encode_png(size, size, px)
        outdir = os.path.join(res, d)
        os.makedirs(outdir, exist_ok=True)
        out = os.path.join(outdir, "ic_launcher.png")
        open(out, "wb").write(png)
        print(f"  {out}  {size}x{size}  {len(png)} 字节")


if __name__ == "__main__":
    main()
