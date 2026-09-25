#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 web/ 下的三个"单文件版"页面重新拼出来。

背景：除了服务端渲染的 review.html / download.html，项目还需要几份**完全自包含**的单文件页面
（内联 css + js，不依赖任何外部资源）。以前这些文件是手工复制维护的，结果和 app.js 漂移了
将近 100 行（加了 HEVC 转码那一整套却没同步过去）。这个脚本只做**纯文本拼接**，
绝不改写 app.js 正文，所以重复跑多少次结果都一样。

产物：
  web/standalone.html       ← web/review.html   + style.css + app.js
  web/standalone_dl.html    ← web/download.html + style.css + app.js
  web_demo/demo.html        ← web/review.html   + style.css + app.js
                              + demo_data.js + demo_prelude.js（假后端）

用法：
  python tools/build_single.py            # 用现有的 web_demo/demo_data.js 重新生成
  python tools/build_single.py --bake     # 另起一个临时服务，重新快照数据到 demo_data.js

注意：web/ 是 go:embed 进 exe 的，改完这里必须重新 go build。
"""

import argparse
import http.client
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)              # medreview/
WEB = os.path.join(ROOT, "web")
DEMO_DIR = os.path.join(ROOT, "web_demo")

STYLE_TAG = '<link rel="stylesheet" href="/style.css">'
SCRIPT_TAG = '<script src="/app.js"></script>'


def read(p):
    with io.open(p, encoding="utf-8") as f:
        return f.read()


def write(p, s):
    os.makedirs(os.path.dirname(p), exist_ok=True)
    # 统一 LF，避免不同平台下产物字节不一致、每次跑都"有改动"
    with io.open(p, "w", encoding="utf-8", newline="\n") as f:
        f.write(s)
    return len(s.encode("utf-8"))


def inline(page_html, css, js, title=None):
    """把 <link href=style.css> 换成 <style>，把 <script src=app.js> 换成 <script>。"""
    if STYLE_TAG not in page_html:
        raise SystemExit("页面里找不到 %s —— review.html/download.html 的结构变了吗？" % STYLE_TAG)
    if SCRIPT_TAG not in page_html:
        raise SystemExit("页面里找不到 %s" % SCRIPT_TAG)
    out = page_html.replace(STYLE_TAG, "<style>\n" + css + "</style>")
    out = out.replace(SCRIPT_TAG, "<script>\n" + js + "</script>")
    if title:
        out = re.sub(r"<title>.*?</title>", "<title>%s</title>" % title, out, count=1)
    return out


def build_standalone(css, js):
    made = []
    for src, dst in (("review.html", "standalone.html"), ("download.html", "standalone_dl.html")):
        n = write(os.path.join(WEB, dst), inline(read(os.path.join(WEB, src)), css, js))
        made.append((dst, n))
    return made


def build_demo(css, js):
    """演示页 = review.html 的 DOM + style.css + app.js + 两个演示专用脚本。

    两个演示脚本必须插在 app.js **之前**：prelude 要先把 fetch / EventSource 换掉，
    app.js 才会走假后端。
    """
    if not os.path.isfile(os.path.join(DEMO_DIR, "demo_data.js")):
        raise SystemExit("缺少 web_demo/demo_data.js，先跑一次 --bake")
    if not os.path.isfile(os.path.join(DEMO_DIR, "demo_prelude.js")):
        raise SystemExit("缺少 web_demo/demo_prelude.js")

    page = read(os.path.join(WEB, "review.html"))
    probe = '<script src="/app.js"></script>'
    demo_scripts = (
        "<script>\n" + read(os.path.join(DEMO_DIR, "demo_data.js")) + "</script>\n"
        "<script>\n" + read(os.path.join(DEMO_DIR, "demo_prelude.js")) + "</script>\n"
        + probe
    )
    page = page.replace(probe, demo_scripts)
    page = page.replace('<script>initApp("review");</script>',
                        '<script>initApp("review");</script>\n'
                        '<!-- 演示页：没有服务端，所有请求都由 demo_prelude.js 假实现应答 -->')
    html = inline(page, css, js, title="素材审阅 · 离线演示")
    n = write(os.path.join(DEMO_DIR, "demo.html"), html)
    return [("web_demo/demo.html", n)]


# ---------------------------------------------------------------- --bake
def http_get(port, path, timeout=20):
    """直连 127.0.0.1，绕开系统代理（本机常年挂 7897，走代理会打不到 loopback）。"""
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        c.request("GET", path, headers={"X-User": "bake"})
        r = c.getresponse()
        body = r.read()
        return r.status, body
    finally:
        c.close()


def bake(root, port=8199):
    """另起一个隔离的临时服务快照数据。

    关键：这个临时进程的 cwd 必须是**临时目录**。如果 cwd 是项目目录，
    它启动时会把自己的端口和随机口令写进 urls.txt，覆盖掉正式实例的下载地址。
    """
    exe = os.path.join(ROOT, "medreview.exe")
    if not os.path.isfile(exe):
        raise SystemExit("找不到 %s，先 go build" % exe)
    if not os.path.isdir(root):
        raise SystemExit("素材目录不存在: %s" % root)

    tmp = tempfile.mkdtemp(prefix="medreview_bake_")
    logf = open(os.path.join(tmp, "out.log"), "wb")
    proc = subprocess.Popen(
        [exe, "-root", root, "-addr", "127.0.0.1:%d" % port,
         "-db", os.path.join(tmp, "bake.db"), "-cache", os.path.join(tmp, "cache"),
         "-vkeep", "-vjobs", "1"],
        cwd=tmp, stdout=logf, stderr=subprocess.STDOUT,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    try:
        # 等扫描结束：/api/status 里 scan.running 变 false，且 files > 0
        st = None
        for _ in range(120):
            time.sleep(1)
            try:
                code, body = http_get(port, "/api/status")
                if code != 200:
                    continue
                st = json.loads(body)
                sc = st.get("scan") or {}
                if not sc.get("running") and sc.get("files"):
                    break
            except Exception:
                continue
        if not st:
            raise SystemExit("临时服务没起来，看日志: %s" % os.path.join(tmp, "out.log"))

        _, body = http_get(port, "/api/folders?parent=0")
        folders0 = json.loads(body)
        folder_ids = [f["id"] for f in folders0.get("folders", [])]
        # 子目录一层（演示数据够用了）
        for fid in list(folder_ids):
            _, b2 = http_get(port, "/api/folders?parent=%d" % fid)
            for f in json.loads(b2).get("folders", []):
                folder_ids.append(f["id"])

        folders = {"0": folders0}
        folder_info = {}
        files = {}
        media_map = {}
        total_files = 0
        for fid in folder_ids:
            _, b = http_get(port, "/api/folders?parent=%d" % fid)
            folders[str(fid)] = json.loads(b)
            _, b = http_get(port, "/api/folder?folder=%d" % fid)
            folder_info[str(fid)] = json.loads(b)
            _, b = http_get(port, "/api/files?folder=%d&limit=300" % fid)
            fl = json.loads(b)
            fl["files"] = [x for x in fl.get("files", []) if x.get("kind") == 1]  # 演示页只用图片
            files[str(fid)] = fl
            for x in fl["files"]:
                media_map[str(x["id"])] = x["name"]
            total_files += len(fl["files"])

        if total_files == 0:
            raise SystemExit("临时服务没索引到图片，检查 -root 目录")

        baked = {"status": st, "folders": folders, "folder": folder_info, "files": files}
        out = [
            "// 演示页烤好的数据快照（由 tools/build_single.py --bake 生成）。",
            "// 说明：web_demo/demo.html 是 build_single.py 从 web/review.html 生成的，",
            "//       这个文件只负责提供数据，不需要手工改 demo.html。",
            "",
            "// id -> 原始文件名；app.js 的 U() 用它把 /api/media?id=N 换成相对路径的原图",
            "window.__DEMO_IMG__ = " + json.dumps(media_map, ensure_ascii=False, sort_keys=True) + ";",
            "",
            "// 各接口的应答快照，被 demo_prelude.js 里的 fetch 假实现使用",
            "window.__DEMO_BAKED__ = " + json.dumps(baked, ensure_ascii=False) + ";",
            "",
        ]
        p = os.path.join(DEMO_DIR, "demo_data.js")
        n = write(p, "\n".join(out))
        print("[bake] 已写入 %s（%d 字节，%d 张图片）" % (p, n, total_files))
        return total_files
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        logf.close()
        shutil.rmtree(tmp, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser(description="重新生成三份自包含单文件页面")
    ap.add_argument("--bake", action="store_true", help="另起隔离的临时服务，重新快照演示数据")
    ap.add_argument("--root", default="", help="--bake 时用的素材目录（默认读 demo_data.js 里记录的）")
    ap.add_argument("--port", type=int, default=8199, help="--bake 用的端口")
    args = ap.parse_args()

    if args.bake:
        root = args.root
        if not root:
            dp = os.path.join(DEMO_DIR, "demo_data.js")
            if os.path.isfile(dp):
                m = re.search(r'"root":\s*"((?:[^"\\]|\\.)*)"', read(dp))
                if m:
                    root = json.loads('"%s"' % m.group(1))
        if not root:
            raise SystemExit("--bake 需要 --root，或让 demo_data.js 里已有 root")
        print("[bake] 素材目录: %s" % root)
        bake(root, args.port)

    css = read(os.path.join(WEB, "style.css"))
    js = read(os.path.join(WEB, "app.js"))
    for name, n in build_standalone(css, js) + build_demo(css, js):
        print("[build] %-24s %d 字节" % (name, n))
    print("完成。web/ 是 go:embed 的，记得重新 go build。")


if __name__ == "__main__":
    main()
