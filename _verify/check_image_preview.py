#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_image_preview.py —— 图片低清预览验收（对部署 exe 跑）

覆盖：
  A. 大图预览：/api/media 给转码 JPEG（短边 1600），文件显著变小
  B. EXIF 方向：orientation=6 的图转出来是竖版（ffmpeg 转正像素）
  C. 缓存：cache/iprev/ 落产物；二次请求字节一致
  D. 小图：短边未超上限 → 原文件直出，不占缓存
  E. orig=1：原文件逐字节一致
  F. dl=1 口径不变：未标记 403 → 标保留后原文件
  G. PNG / 损坏文件：PNG 转预览；损坏文件回退原图不 500
  H. zip 打包仍只含保留文件
  I. -ires 改档：重启后预览变 800，旧缓存被清空（.profile 换标签）
  J. 源码断言：zip 客户端中断降噪、image.go 关键结构

注意：需要 Pillow（读图片尺寸/EXIF），用 venv 的 python 跑：
  %USERPROFILE%\.workbuddy\binaries\python\envs\default\Scripts\python.exe

自造素材（ffmpeg 合成）、自起停服务（独立端口 8105）、隔离 cwd（不碰项目 urls.txt）。
任何 FAIL → 退出码非 0。
"""
import os
import json
import shutil
import struct
import subprocess
import sys
import time
import urllib.request
import urllib.error
import zipfile
from io import BytesIO

HERE = os.path.dirname(os.path.abspath(__file__))
EXE = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "..", "medreview.exe")
EXE = os.path.abspath(EXE)
FFMPEG = os.path.join(HERE, "..", "tools", "ffmpeg", "ffmpeg.exe")
FFMPEG = os.path.abspath(FFMPEG)
PORT = 8105
BASE = "http://127.0.0.1:%d" % PORT
TOKEN = "imgpreview-t0ken"
RUN = os.path.join(HERE, "_run_img")
SRC = os.path.join(RUN, "src")
PY_PIL = True
try:
    from PIL import Image, ImageStat
except ImportError:
    PY_PIL = False

PASS = 0
FAIL = 0

def check(name, ok, detail=""):
    global PASS, FAIL
    if ok:
        PASS += 1
        print("PASS %s" % name)
    else:
        FAIL += 1
        print("FAIL %s  %s" % (name, detail))

# ---------- 本机安全机制：不真删，改名让路 ----------
def forget_dir(d):
    if not os.path.exists(d):
        return
    trash = d + "_trash_%d" % int(time.time())
    os.replace(d, trash)

def fresh_dir(d):
    forget_dir(d)
    os.makedirs(d, exist_ok=True)

def http_get(path, raw=False):
    url = BASE + path
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(url, timeout=60) as r:
            data = r.read()
            return r.status, data if raw else data, dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)

def http_post_json(path, obj, user="verifier"):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    body = json.dumps(obj).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=body,
        headers={"Content-Type": "application/json", "X-User": user}, method="POST")
    try:
        with opener.open(req, timeout=30) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()

def wait_status(timeout=30):
    dl = time.time() + timeout
    while time.time() < dl:
        try:
            code, _, _ = http_get("/api/status?t=" + TOKEN)
            if code == 200:
                return True
        except Exception:
            pass
        time.sleep(0.2)
    return False

def wait_scan(expect_files, timeout=60):
    dl = time.time() + timeout
    while time.time() < dl:
        try:
            code, body, _ = http_get("/api/status?t=" + TOKEN)
            if code == 200:
                import json
                st = json.loads(body.decode("utf-8"))
                sc = st.get("scan", {})
                if sc.get("running") is False and sc.get("endedAt", 0) > 0:
                    return True
        except Exception:
            pass
        time.sleep(0.25)
    return False

def jpg_dims(data):
    if not PY_PIL:
        return None
    im = Image.open(BytesIO(data))
    return im.size

def start_server(ires):
    args = [EXE, "-root", SRC, "-token", TOKEN, "-addr", ":%d" % PORT,
            "-db", os.path.join(RUN, "medreview.db"), "-cache", os.path.join(RUN, "cache"),
            "-ires", str(ires)]
    logf = open(os.path.join(RUN, "server_i%d.log" % ires), "ab")
    return subprocess.Popen(args, cwd=RUN, stdout=logf, stderr=logf)

def stop_server(proc):
    try:
        proc.terminate()
        proc.wait(timeout=10)
    except Exception:
        try:
            proc.kill()
        except Exception:
            pass

# ---------- 素材 ----------
def make_assets():
    fresh_dir(SRC)
    big = os.path.join(SRC, "big.jpg")
    rot = os.path.join(SRC, "rot.jpg")
    small = os.path.join(SRC, "small.jpg")
    png = os.path.join(SRC, "big.png")
    fake = os.path.join(SRC, "fake.jpg")
    subprocess.run([FFMPEG, "-y", "-v", "error", "-f", "lavfi",
                    "-i", "testsrc2=size=4000x3000", "-frames:v", "1", big],
                   check=True, timeout=120)
    subprocess.run([FFMPEG, "-y", "-v", "error", "-f", "lavfi",
                    "-i", "testsrc2=size=3000x2000", "-frames:v", "1", png],
                   check=True, timeout=120)
    subprocess.run([FFMPEG, "-y", "-v", "error", "-f", "lavfi",
                    "-i", "testsrc2=size=800x600", "-frames:v", "1", small],
                   check=True, timeout=120)
    if PY_PIL:
        im = Image.open(big)
        ex = im.getexif(); ex[0x0112] = 6  # 竖版显示
        im.save(rot, exif=ex)
    else:
        shutil.copyfile(big, rot)
    with open(fake, "wb") as f:
        f.write(b"\xff\xd8\xffthis is not a real jpeg at all - corrupt payload")
    return {"big.jpg": big, "rot.jpg": rot, "small.jpg": small,
            "big.png": png, "fake.jpg": fake}

def file_ids():
    import json
    code, body, _ = http_get("/api/files?folder=1&limit=50&t=" + TOKEN)
    assert code == 200, "files 接口失败 %s" % code
    files = json.loads(body.decode("utf-8")).get("files", [])
    out = {}
    for it in files:
        out[it.get("name")] = it.get("id")
    return out

def iprev_products():
    d = os.path.join(RUN, "cache", "iprev")
    if not os.path.isdir(d):
        return 0
    n = 0
    for root, _dirs, fs in os.walk(d):
        for f in fs:
            if f.endswith(".jpg"):
                n += 1
    return n

def main():
    check("Pillow 可用（读尺寸/EXIF 必需）", PY_PIL)
    if not PY_PIL:
        return 1
    fresh_dir(RUN)  # 整个 RUN（含 cache）改名让路：图片预览缓存跨启动保留，必须从头干净
    assets = make_assets()
    orig_bytes = {n: open(p, "rb").read() for n, p in assets.items()}

    proc = start_server(1600)
    try:
        check("服务启动（/api/status）", wait_status())
        check("扫描完成（4 个文件）", wait_scan(4))
        ids = file_ids()
        for n in ["big.jpg", "rot.jpg", "small.jpg", "big.png", "fake.jpg"]:
            check("files 列表含 %s" % n, n in ids, str(ids.keys()))

        # A. 大图预览
        code, data, hdr = http_get("/api/media?id=%d&t=%s" % (ids["big.jpg"], TOKEN))
        check("big.jpg 预览 200", code == 200, str(code))
        check("big.jpg 预览是 JPEG", data[:2] == b"\xff\xd8", repr(data[:4]))
        check("big.jpg 预览显著变小", len(data) < len(orig_bytes["big.jpg"]) * 0.5,
              "%d vs %d" % (len(data), len(orig_bytes["big.jpg"])))
        w, h = jpg_dims(data)
        check("big.jpg 预览短边=1600", min(w, h) == 1600, "%dx%d" % (w, h))
        check("big.jpg 预览长边≈2133", abs(max(w, h) - 2133) <= 2, "%dx%d" % (w, h))

        # B. EXIF 方向
        code, rdata, _ = http_get("/api/media?id=%d&t=%s" % (ids["rot.jpg"], TOKEN))
        rw, rh = jpg_dims(rdata)
        check("rot.jpg 预览是竖版（EXIF 转正）", rh > rw, "%dx%d" % (rw, rh))
        check("rot.jpg 预览短边=1600", min(rw, rh) == 1600, "%dx%d" % (rw, rh))

        # 等目录预热收敛（进目录即预热：big/rot/png 会被后台转好，small/fake 不产缓存）
        dl = time.time() + 20
        while time.time() < dl and iprev_products() < 3:
            time.sleep(0.3)

        # C. 缓存（预热完成后 big/rot/png = 3 个）
        n_prod = iprev_products()
        check("进目录预热完成（big/rot/png = 3 个）", n_prod == 3, str(n_prod))
        code2, data2, _ = http_get("/api/media?id=%d&t=%s" % (ids["big.jpg"], TOKEN))
        check("二次请求字节一致（缓存命中）", data2 == data)

        # D. 小图直出
        code, sdata, _ = http_get("/api/media?id=%d&t=%s" % (ids["small.jpg"], TOKEN))
        check("small.jpg 原文件直出", sdata == orig_bytes["small.jpg"])
        check("small.jpg 不占缓存", iprev_products() == 3, str(iprev_products()))

        # E. orig=1
        code, odata, _ = http_get("/api/media?id=%d&t=%s&orig=1" % (ids["big.jpg"], TOKEN))
        check("orig=1 给原文件", odata == orig_bytes["big.jpg"])

        # F. dl=1 口径
        code, _, _ = http_get("/api/media?id=%d&t=%s&dl=1" % (ids["big.jpg"], TOKEN))
        check("未标记 dl=1 → 403", code == 403, str(code))
        http_post_json("/api/review?t=" + TOKEN,
                       {"fileId": ids["big.jpg"], "decision": 1})
        code, dldata, hdr = http_get("/api/media?id=%d&t=%s&dl=1" % (ids["big.jpg"], TOKEN))
        check("标保留后 dl=1 → 200", code == 200, str(code))
        check("dl=1 是原文件", dldata == orig_bytes["big.jpg"])
        check("dl=1 带 attachment 头", "attachment" in hdr.get("Content-Disposition", ""))

        # G. PNG 与损坏文件
        code, pdata, hdr = http_get("/api/media?id=%d&t=%s" % (ids["big.png"], TOKEN))
        check("png 预览转成 JPEG", pdata[:2] == b"\xff\xd8", repr(pdata[:4]))
        pw, ph = jpg_dims(pdata)
        check("png 预览短边=1600", min(pw, ph) == 1600, "%dx%d" % (pw, ph))
        check("png 预览在缓存里（共 3 个，含预热）", iprev_products() == 3, str(iprev_products()))
        code, kdata, _ = http_get("/api/media?id=%d&t=%s" % (ids["fake.jpg"], TOKEN))
        check("损坏文件回退原图（200 且字节一致）",
              code == 200 and kdata == orig_bytes["fake.jpg"], str(code))

        # H. zip 仍只含保留文件
        code, zdata, _ = http_get("/api/zip?folder=1&t=" + TOKEN)
        check("zip 200", code == 200, str(code))
        zf = zipfile.ZipFile(BytesIO(zdata))
        names = zf.namelist()
        # 2026-09-25：打包会附带 导出清单.csv（有意新增）
        check("zip 只含保留的 big.jpg（+导出清单）", sorted(names) == ["big.jpg", "导出清单.csv"], str(names))
        check("zip 内文件字节一致", zf.read("big.jpg") == orig_bytes["big.jpg"])

        # J1. 源码断言（对部署版无法直接验证日志行为，用源码防漂移）
        src_zip = open(os.path.join(HERE, "..", "zip.go"), encoding="utf-8").read()
        check("zip.go 有 isClientAbort", "func isClientAbort" in src_zip)
        check("zip.go 客户端中断降噪文案", "客户端中断下载" in src_zip)
        check("zip.go 不再有旧文案", "zip 收尾失败（客户端可能已中断）" not in src_zip)
        src_main = open(os.path.join(HERE, "..", "main.go"), encoding="utf-8").read()
        check("main.go -ires 默认 1600", '"ires", 1600' in src_main)
        src_img = open(os.path.join(HERE, "..", "image.go"), encoding="utf-8").read()
        check("image.go 有 PrepareCache 规格检查", ".profile" in src_img)
        check("image.go 有 EXIF 方向处理", "jpegOrientation" in src_img)
        check("image.go 转码失败回退原图", "回退原图" in src_img)
        check("image.go 有目录预热（WarmFolder + 取消上一轮）",
              "func (s *ImageService) WarmFolder" in src_img and "warmCancel" in src_img)
        src_api = open(os.path.join(HERE, "..", "api.go"), encoding="utf-8").read()
        check("handleFiles 触发目录预热", "WarmFolder(folderID)" in src_api)
        src_app = open(os.path.join(HERE, "..", "web", "app.js"), encoding="utf-8").read()
        check("灯箱视频直接挂到定高容器（修忽大忽小）",
              "box.remove();" in src_app and "el.lbBody.appendChild(v);" in src_app and
              "v.style.maxHeight" not in src_app)
        src_html_r = open(os.path.join(HERE, "..", "web", "review.html"), encoding="utf-8").read()
        src_html_d = open(os.path.join(HERE, "..", "web", "download.html"), encoding="utf-8").read()
        check("review.html 有 lb-orig", 'id="lb-orig"' in src_html_r)
        check("download.html 有 lb-orig", 'id="lb-orig"' in src_html_d)
        check("两份 HTML lb-orig 逐字一致",
              src_html_r.count('id="lb-orig"') == src_html_d.count('id="lb-orig"') == 1)
        src_app = open(os.path.join(HERE, "..", "web", "app.js"), encoding="utf-8").read()
        # 以前翻页是写死回预览版；加了画质开关之后改成回到开关的默认值（defaultLbOrig）
        check("app.js lbOrig 翻页复位（回到画质开关的默认值）",
              "S.lbOrig = defaultLbOrig();" in src_app and "function defaultLbOrig(" in src_app)
        check("app.js orig=1 拼参", '"&orig=1"' in src_app)
    finally:
        stop_server(proc)

    # I. 改档重启
    proc = start_server(800)
    try:
        check("重启（-ires 800）", wait_status())
        check("重启后扫描完成", wait_scan(4))
        ids = file_ids()
        code, data8, _ = http_get("/api/media?id=%d&t=%s" % (ids["big.jpg"], TOKEN))
        w8, h8 = jpg_dims(data8)
        check("改档后预览短边=800", min(w8, h8) == 800, "%dx%d" % (w8, h8))
        prof = open(os.path.join(RUN, "cache", "iprev", ".profile"), "rb").read()
        check(".profile 换成 i800", prof.strip() == b"i800", repr(prof))
        code, odata, _ = http_get("/api/media?id=%d&t=%s&orig=1" % (ids["big.jpg"], TOKEN))
        check("改档后 orig=1 仍是原文件", odata == orig_bytes["big.jpg"])
    finally:
        stop_server(proc)

    print("\n==== %d PASS / %d FAIL ====" % (PASS, FAIL))
    return 1 if FAIL else 0

if __name__ == "__main__":
    sys.exit(main())
