"""打便携包：medreview.exe + start.bat 一起打进去。

用户 2026-10-10 明确要求：包里要有批处理文件。start.bat 提供交互式启动
（输入素材目录 / 下载口令 / 端口 / QC 是否用显卡解码），也支持把文件夹
拖到 bat 图标上直接启动。

注意 bat 必须在包里保持 **GBK 编码** —— 中文 Windows 控制台代码页是 GBK，
UTF-8 的中文会显示成乱码（.bat 的硬规矩，Go/Python 源码不受此限）。
"""
import os
import shutil
import zipfile

EXE = "medreview.exe"
BAT = "start.bat"
DST = "dist/medreview-portable-v1.2.0.zip"
# 包里的批处理用这个名字（用户习惯中文名；内容仍是纯 ASCII 提示，
# 文件名是 UTF-8 路径，zip 本身支持，不受 GBK 影响）
BAT_IN_ZIP = "启动审阅.bat"

os.makedirs("dist", exist_ok=True)
print("packaging ->", DST)

with zipfile.ZipFile(DST, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as z:
    z.write(EXE, EXE)
    if os.path.exists(BAT):
        # start.bat 是 GBK：按原样以二进制塞进 zip，绝不能经过 UTF-8 转换
        z.write(BAT, BAT_IN_ZIP)
        print("  included:", BAT, "->", BAT_IN_ZIP)
    else:
        raise SystemExit("ERROR: %s not found (portable package must ship a .bat)" % BAT)

print("done size=", os.path.getsize(DST))

# 自检：确认 bat 在包里且字节数非 0
with zipfile.ZipFile(DST) as z:
    names = z.namelist()
    assert BAT_IN_ZIP in names, "bat missing in zip: %s" % names
    assert z.getinfo(BAT_IN_ZIP).file_size > 0, "bat is empty in zip"
    assert z.testzip() is None, "zip corrupt"
    print("verified: %s (%d bytes) + %s" % (EXE, z.getinfo(EXE).file_size,
                                            BAT_IN_ZIP))