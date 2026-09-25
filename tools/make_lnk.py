#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""纯 Python 生成 .lnk 快捷方式（不依赖 WScript.Shell / COM，本机 PowerShell 的 COM 被安全策略拦了）。

用法：
  python tools/make_lnk.py            # 在 medreview/ 下生成 medreview_ui.lnk 和 medreview.lnk
  python tools/make_lnk.py --check    # 生成后顺手解析校验（自测用）

生成的快捷方式：
  · TargetPath 指向本目录里的 exe（绝对路径，含中文也能正确存为 Unicode）
  · WorkingDirectory 指向本目录（urls.txt / medreview.db / medreview.log 都写在这儿）
  · 把文件夹拖到图标上 → Windows 会把拖入的路径作为启动参数追加，正好被 main.go 的位置参数接住
"""
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)


def _ustr(s):
    """StringData 里的 Unicode 串：2 字节长度（UTF-16 码元数）+ UTF-16LE + 双字节 null 结尾。"""
    b = s.encode("utf-16-le") + b"\x00\x00"
    return struct.pack("<H", len(b) // 2) + b


def make_lnk(lnk_path, target, working_dir, name):
    target_w = target.replace("/", "\\")
    wd_w = working_dir.replace("/", "\\")

    # ---- LinkInfo：本地路径，含 Unicode 版（路径可能带中文，ANSI 会丢，必须上 UTF-16 版）----
    ansi = target_w.encode("ascii", "replace")  # 占位，真实路径靠下面的 Unicode 版
    uni = target_w.encode("utf-16-le") + b"\x00\x00"

    # VolumeID（空卷标）
    vol = struct.pack("<I", 3)        # DriveType: fixed
    vol += struct.pack("<I", 0)       # SerialNumber
    vol += struct.pack("<I", 16)      # VolumeLabelOffset（相对 VolumeID 起点）
    vol += b"\x00\x00"                # 空卷标
    vol = struct.pack("<I", len(vol) + 4) + vol

    local_off = 28 + len(vol)         # LinkInfo 起点 + HeaderSize(28) + vol 长度
    li = struct.pack("<I", 28)        # LinkInfoHeaderSize
    li += struct.pack("<I", 1)        # Flags: VolumeIDAndLocalBasePath
    li += struct.pack("<I", 28)       # VolumeIDOffset
    li += struct.pack("<I", local_off)  # LocalBasePathOffset
    li += struct.pack("<I", 0)        # CommonNetworkRelativeLinkOffset
    li += struct.pack("<I", 0)        # CommonNetworkRelativeLinkSize
    li += vol
    li += ansi + b"\x00"
    li += uni
    li = struct.pack("<I", len(li) + 4) + li   # LinkInfoSize 前缀

    # ---- StringData：Name + WorkingDir（按 LinkFlags 位序，跳过未设置的项）----
    string_data = _ustr(name) + _ustr(wd_w)

    # ---- Header ----
    flags = 0x2 | 0x4 | 0x10 | 0x80   # HasLinkInfo | HasName | HasWorkingDir | IsUnicode
    header = struct.pack("<I", 76)     # HeaderSize
    header += bytes.fromhex("0114020000000000C000000000000046")  # ShellLink CLSID
    header += struct.pack("<I", flags)
    header += struct.pack("<I", 0)     # FileAttributes
    header += b"\x00" * 24             # CreationTime / AccessTime / WriteTime
    header += struct.pack("<I", 0)     # FileSize
    header += struct.pack("<I", 0)     # IconIndex
    header += struct.pack("<I", 1)     # ShowCommand: normal
    header += b"\x00" * 16             # 3 个 reserved 字段

    with open(lnk_path, "wb") as f:
        f.write(header + li + string_data)
    return lnk_path


def _check(lnk_path):
    with open(lnk_path, "rb") as f:
        data = f.read()
    assert data[:4] == b"\x4c\x00\x00\x00", "HeaderSize 不是 76"
    cls = data[4:20].hex()
    assert cls == "0114020000000000c000000000000046", "CLSID 不对"
    flags = struct.unpack("<I", data[20:24])[0]
    assert flags & 0x2 and flags & 0x4 and flags & 0x10 and flags & 0x80, "缺少必要的 LinkFlags"
    print(f"  [OK] {os.path.basename(lnk_path)}  {len(data)} 字节, flags=0x{flags:04x}")


def main():
    ui = os.path.join(ROOT, "medreview_ui.exe")
    con = os.path.join(ROOT, "medreview.exe")
    make_lnk(os.path.join(ROOT, "medreview_ui.lnk"), ui, ROOT, "medreview_ui")
    make_lnk(os.path.join(ROOT, "medreview.lnk"), con, ROOT, "medreview")
    print("已生成快捷方式：")
    if "--check" in sys.argv:
        _check(os.path.join(ROOT, "medreview_ui.lnk"))
        _check(os.path.join(ROOT, "medreview.lnk"))


if __name__ == "__main__":
    main()
