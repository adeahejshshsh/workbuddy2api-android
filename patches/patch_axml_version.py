#!/usr/bin/env python3
"""patch_axml_version.py — 就地改写二进制 AndroidManifest.xml 的 versionCode。

为什么需要（issue B8）：没有 aapt2 的降级构建复用上一版 APK 的清单，只把
versionName 字符串等长替换 —— 打出来的包 versionName 是新的、versionCode 还是旧的，
覆盖安装时 Android 会认为"版本没变/更旧"，可能拒绝安装或让用户以为没更新。

做法：解析 AXML 的 resource map，找到 `android:versionCode`（资源 id 0x0101021b）
的 int 属性，把 4 字节小端数据原地改掉。改完**回读校验**，不通过就以非 0 退出
（宁可不产出，也不产出半正确的包）。

用法: python3 patch_axml_version.py <manifest.xml> <newVersionCode>
"""
import struct
import sys

RES_VERSION_CODE = 0x0101021B
TYPE_INT_DEC = 0x10


def patch(path, new_code):
    with open(path, "rb") as f:
        buf = bytearray(f.read())
    if len(buf) < 8:
        raise SystemExit("manifest 太小，不是 AXML")

    # 1) 遍历 chunk，建立 资源id ← 字符串池下标 的映射
    resmap = []
    off = 8  # 跳过 XML 文件头（type + headerSize + size）
    end = len(buf)
    while off + 8 <= end:
        ctype, hsize, csize = struct.unpack_from("<HHI", buf, off)
        if csize <= 0 or off + csize > end:
            break
        if ctype == 0x0180:  # RES_XML_RESOURCE_MAP
            n = (csize - hsize) // 4
            resmap = list(struct.unpack_from("<%dI" % n, buf, off + hsize))
        off += csize

    if not resmap:
        raise SystemExit("找不到 resource map chunk")

    # 2) 在 START_ELEMENT 里找 versionCode 属性
    hits = []
    off = 8
    while off + 8 <= end:
        ctype, hsize, csize = struct.unpack_from("<HHI", buf, off)
        if csize <= 0 or off + csize > end:
            break
        if ctype == 0x0102:  # RES_XML_START_ELEMENT
            # 头部：chunk(8) + lineNumber(4) + comment(4) + ns(4) + name(4)
            #       + attributeStart(2) + attributeSize(2) + attributeCount(2)
            #       + idIndex(2) + classIndex(2) + styleIndex(2)
            attr_start, attr_size, attr_count = struct.unpack_from("<HHH", buf, off + hsize + 8)
            base = off + hsize + attr_start
            for i in range(attr_count):
                a = base + i * attr_size
                if a + 20 > off + csize:
                    break
                _ns, name_idx, _raw = struct.unpack_from("<III", buf, a)
                data_type = buf[a + 15]
                data = struct.unpack_from("<I", buf, a + 16)[0]
                rid = resmap[name_idx] if name_idx < len(resmap) else 0
                if rid == RES_VERSION_CODE and data_type == TYPE_INT_DEC:
                    hits.append((a + 16, data))
        off += csize

    if len(hits) != 1:
        raise SystemExit("versionCode 属性定位失败（命中 %d 处）" % len(hits))

    where, old = hits[0]
    if old == new_code:
        print("  versionCode 已是 %d，无需改写" % new_code)
        return
    struct.pack_into("<I", buf, where, new_code)

    # 3) 回读校验
    with open(path, "wb") as f:
        f.write(buf)
    with open(path, "rb") as f:
        chk = bytearray(f.read())
    now = struct.unpack_from("<I", chk, where)[0]
    if now != new_code:
        raise SystemExit("回读校验失败：写入 %d 但读到 %d" % (new_code, now))
    print("  versionCode %d → %d（AXML 原地改写 + 回读校验通过）" % (old, new_code))


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("用法: patch_axml_version.py <AndroidManifest.xml> <newVersionCode>")
    patch(sys.argv[1], int(sys.argv[2]))
