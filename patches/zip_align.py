#!/usr/bin/env python3
"""zip_align.py —— 把 resources.arsc 以 STORED 写入并对齐到 4 字节。

Android R+（API 30）的包安装器会硬性检查：

    resources.arsc must be stored uncompressed and aligned on a 4-byte boundary

否则报 `INSTALL_PARSE_FAILED_RESOURCES_ARSC_COMPRESSED`
（你看到的 `-124 ... Targeting R+ ...` 就是这条）。

`zipfile` 默认把所有条目 ZIP_DEFLATED，而且按自己的顺序计算偏移，没有「对齐写入」
这个能力。这里手工补：写入前算出数据段偏移，用合法的 extra 字段（TLV）把长度垫到
4 的倍数 —— extra 是 zip 标准的一部分，安装器不会因此报错。

用法: python3 zip_align.py <in.apk> <out.apk>
"""
import struct
import sys
import zipfile

ALIGN = 4
ARSC = "resources.arsc"
# 0xd935 是 Android 生态里 zipalign 用的 padding 头 id（任意合法 id 均可，取它便于识别）
PAD_ID = 0xD935


def _extra_to(tell, name, intended):
    """返回能让数据段对齐到 ALIGN 的 extra 字节串（至少 4 字节 TLV 或空）。"""
    base = tell + 30 + len(name.encode()) + intended
    pad = (ALIGN - (base % ALIGN)) % ALIGN
    if pad == 0:
        return b""
    # extra 必须是完整 TLV（id:2 + len:2 + data），最少 4 字节；长度再加一轮 ALIGN
    # 不改变同余，所以 pad<4 时补成 pad+ALIGN。
    size = pad if pad >= 4 else pad + ALIGN
    return struct.pack("<HH", PAD_ID, size - 4) + b"\x00" * (size - 4)


def main(src, dst):
    zin = zipfile.ZipFile(src)
    zout = zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED, allowZip64=False)
    stored = 0
    for it in zin.infolist():
        data = zin.read(it.filename)
        name = it.filename
        zi = zipfile.ZipInfo(name, date_time=it.date_time)
        zi.external_attr = it.external_attr
        zi.create_system = it.create_system
        if name == ARSC:
            zi.compress_type = zipfile.ZIP_STORED
            zi.extra = _extra_to(zout.fp.tell(), name, 0)
            stored += 1
        else:
            zi.compress_type = zipfile.ZIP_DEFLATED
            zi.extra = b""
        zout.writestr(zi, data)
    zout.close()
    zin.close()
    print("  对齐写入: %s（STORED=%d）" % (dst, stored))


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("用法: zip_align.py <in.apk> <out.apk>")
    main(sys.argv[1], sys.argv[2])
