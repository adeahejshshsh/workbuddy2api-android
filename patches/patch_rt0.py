#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
patch_rt0.py — 给已编译好的 Go(不区分 GOOS) arm64 可执行文件打「argv 修正」补丁。

背景（本机 Android 的限制）：
    本 App targetSdk >= 29 → 内核/SELinux 禁止 exec app 私有目录里的任何文件
    （sdcard 是 noexec 挂载，$PREFIX 是 app data，execve 一律 EACCES）。
    唯一出路是让 /system/bin/linker64「代跑」二进制，此时内核栈布局变成：
        stack = [ 发起方 argv0, 程序绝对路径, 真实参数... ]   （多出 2 格）
    而 bionic libc 会用 initial_linker_arg_count 自行修正 1 格，Go 不会 ——
    Go 的 _rt0_arm64 直接从栈读 argc/argv，于是 os.Args 永远错位一格。

补丁（等长替换，不需要新增空间）：
    ldr x0,[sp]     ; x0 = argc_raw
    add x1,sp,#16   ; argv = sp + 16   （跳过 [argv0, 绝对路径] 两格）
    sub x0,x0,#1    ; argc = argc_raw - 1
    b   rt0_go      ; （吃掉原来的 JMP + 4 字节对齐填充）
用法:
    python3 patch_rt0.py <exe> [<exe> ...]
    幂等：已打过的文件报告 pattern-count=0。
"""
import struct
import sys
import os

PAT = struct.pack('<II', 0xF94003E0, 0x910023E1)   # ldr x0,[sp] ; add x1,sp,#8
ADD16 = 0x910043E1                                  # add x1, sp, #16
SUB1 = 0xD1000400                                   # sub x0, x0, #1


def patch(path):
    with open(path, 'rb') as f:
        d = bytearray(f.read())
    if d[:4] != b'\x7fELF':
        return "skip: not ELF"
    if struct.unpack('<H', d[16:18])[0] != 3:
        return "skip: not ET_DYN (需 PIE 才可能被 linker 加载)"
    idx, i = [], d.find(PAT)
    while i != -1:
        idx.append(i)
        i = d.find(PAT, i + 1)
    if len(idx) == 0:
        return "already-patched (pattern 不存在)"
    if len(idx) != 1:
        return "skip: pattern 命中 %d 次，拒绝盲改" % len(idx)
    o = idx[0]
    w_b, w_after = struct.unpack('<II', d[o + 8:o + 16])
    if (w_b >> 26) != 0b000101:                 # B
        return "skip: 第3条指令不是 B (%#x)" % w_b
    if w_after != 0:
        return "skip: 第4格不是对齐填充 (%#x)，无空间" % w_after
    imm = w_b & 0x3FFFFFF
    if imm & 0x2000000:
        imm -= 0x4000000
    tgt = o + 8 + 4 * imm                       # 原 JMP 目标 = rt0_go
    newb = 0x14000000 | (((tgt - (o + 12)) // 4) & 0x3FFFFFF)
    d[o + 4:o + 8] = struct.pack('<I', ADD16)
    d[o + 8:o + 12] = struct.pack('<I', SUB1)
    d[o + 12:o + 16] = struct.pack('<I', newb)
    with open(path, 'wb') as f:
        f.write(d)
    return "PATCHED @%#x -> argc-1 / argv+16 (b=%#x)" % (o, newb)


if __name__ == '__main__':
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    for p in sys.argv[1:]:
        if not os.path.isfile(p):
            print("%s: 不存在" % p)
            continue
        try:
            print("%s: %s" % (p, patch(p)))
        except Exception as e:
            print("%s: 出错 %s" % (p, e))
