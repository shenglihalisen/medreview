# -*- coding: utf-8 -*-
"""medreview 验收：三种启动方式 + 本地控制台窗口。

对应需求「三合一启动」：
  ① 把文件夹拖到 exe / 快捷方式图标上启动（位置参数当素材根目录）
  ② 无黑窗版（medreview_ui.exe）：双击启动**只弹本地窗口**（现在是 WebView2 网页级
     界面，不再是手搓 Win32 控件），地址/口令/日志都在里面看，还能就地改素材根目录、
     监听地址、下载口令（改完立即生效，不用重启）。不自动开浏览器 ——
     想用浏览器就点窗口里的「打开审阅页 / 打开下载页」。
  ③ 控制台版（medreview.exe）：双击 / 命令行直接跑，弹出黑窗口显示地址和进度

另外覆盖：gui 标志（从 /api/status 取）、网页控制台接口已彻底移除（防回归）。

自给自足：自己造素材、自己起停服务、自己清场，不依赖 ffmpeg。

用法：
    python _verify/check_startup_modes.py                  # 验默认的 medreview.exe + medreview_ui.exe
    python _verify/check_startup_modes.py a.exe b_ui.exe   # 分别指定 console 版 / gui 版
"""

import os
import sys
import json
import time
import subprocess
import urllib.error
import urllib.request

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CON_EXE = os.path.abspath(sys.argv[1]) if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
GUI_EXE = os.path.abspath(sys.argv[2]) if len(sys.argv) > 2 else os.path.join(D, 'medreview_ui.exe')

RUN = os.path.join(D, '_verify', 'startmodes')
MAT_A = os.path.join(RUN, 'matA')
MAT_B = os.path.join(RUN, 'matB')
CACHE = os.path.join(RUN, 'cache')

OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
results = []
PROCS = []
PORT = 8153
TOKEN = 'tk'


def check(name, ok, detail=''):
    results.append((name, bool(ok), str(detail)))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + str(detail)) if detail else ''))
    sys.stdout.flush()


def fresh_dir(path):
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError as e:
            print('  !! 无法让开旧目录 %s: %r' % (path, e))
            raise SystemExit(2)
    os.makedirs(path, exist_ok=True)


def build_fixture():
    os.makedirs(MAT_A, exist_ok=True)
    os.makedirs(MAT_B, exist_ok=True)
    for d, n in ((MAT_A, 'A'), (MAT_B, 'B')):
        for i in range(3):
            p = os.path.join(d, '%s_%d.jpg' % (n, i))
            with open(p, 'wb') as f:
                f.write(b'x' * (97 + i))


def request(port, path, raw=False, data=None, ctype=None, user='verifier', token=None):
    last = None
    url = 'http://127.0.0.1:%d%s' % (port, path)
    if token and '?' not in url:
        url += '?t=' + token
    elif token:
        url += '&t=' + token
    for attempt, wait in enumerate((0.3, 1.0, 2.5), start=1):
        r = urllib.request.Request(url, data=data)
        if ctype:
            r.add_header('Content-Type', ctype)
        r.add_header('X-User', user)
        try:
            resp = OP.open(r, timeout=120)
            b = resp.read()
            return b if raw else json.loads(b.decode('utf-8'))
        except (ConnectionResetError, ConnectionAbortedError) as e:
            last = e
            time.sleep(wait)
        except urllib.error.HTTPError as e:
            return e  # 业务状态码交给调用方
    raise last


def code_of(port, path, token=None, method='GET', data=None):
    r = urllib.request.Request('http://127.0.0.1:%d%s' % (port, path), data=data, method=method)
    if token and '?' not in r.full_url:
        r.full_url += '?t=' + token
    r.add_header('X-User', 'verifier')
    try:
        OP.open(r, timeout=60).read()
        return 200
    except urllib.error.HTTPError as e:
        return e.code


def start(exe, port, extra_args, logname):
    """起一个实例，返回 (proc, server_log_text_path)。cwd=RUN，日志写到 RUN/<logname>。"""
    lf = open(os.path.join(RUN, logname), 'w', encoding='utf-8', newline='')
    args = [exe, '-addr', ':%d' % port, '-token', TOKEN, '-db', 'v.db', '-cache', CACHE] + list(extra_args)
    p = subprocess.Popen(args, cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
    PROCS.append(p)
    for _ in range(120):
        if p.poll() is not None:
            break
        try:
            request(port, '/api/status')
            time.sleep(0.2)
            return p, lf
        except Exception:
            time.sleep(0.3)
    print('  !! 服务未能启动（poll=%r），日志：' % p.poll())
    for l in open(os.path.join(RUN, logname), encoding='utf-8', errors='replace').read().splitlines()[-12:]:
        print('     | ' + l)
    raise RuntimeError('服务未能启动')


def wait_scan(port):
    for _ in range(60):
        try:
            sc = request(port, '/api/status').get('scan') or {}
        except Exception:
            sc = {}
        if (not sc.get('running')) and (sc.get('files') or 0) >= 1 and (sc.get('endedAt') or 0) > 0:
            return
        time.sleep(0.2)


def log_tail(logname, needle):
    """读 server 日志，返回包含 needle 的那行（用于确认根目录路径真的进了日志）。"""
    path = os.path.join(RUN, logname)
    if not os.path.exists(path):
        return None
    for l in open(path, encoding='utf-8', errors='replace').read().splitlines():
        if needle in l:
            return l
    return None


def main():
    fresh_dir(RUN)
    build_fixture()

    # ---------- ① 位置参数（拖文件夹启动）----------
    print('== A. 位置参数 / 拖文件夹启动 ==')
    p1, lf1 = start(CON_EXE, PORT, [MAT_B], 'srv_pos.log')
    wait_scan(PORT)
    line = log_tail('srv_pos.log', '素材根目录')
    check('位置参数：拖入的目录被当作素材根目录', line and MAT_B.replace('/', '\\') in line, line)
    # 不带位置参数（只用 -root）也能启动，根目录仍是显式指定的
    p1.terminate()

    # -root 显式 > 位置参数
    p2, _ = start(CON_EXE, PORT, ['-root', MAT_A, MAT_B], 'srv_root_pref.log')
    wait_scan(PORT)
    line2 = log_tail('srv_root_pref.log', '素材根目录')
    check('显式 -root 优先于位置参数', line2 and MAT_A.replace('/', '\\') in line2 and MAT_B.replace('/', '\\') not in line2, line2)
    p2.terminate()

    # ---------- ② 两种构建的 gui 标志（现从 /api/status 取）----------
    print('== B. console 版 / 无黑窗 gui 版 的 buildMode 标志 ==')
    p3, _ = start(CON_EXE, PORT, [MAT_A], 'srv_console.log')
    st = request(PORT, '/api/status?t=%s' % TOKEN)
    check('console 版 /api/status 报 gui=false', st and st.get('gui') is False, st.get('gui') if isinstance(st, dict) else st)
    p3.terminate()

    p4, _ = start(GUI_EXE, PORT, ['-root', MAT_A, '-open=false'], 'srv_gui.log')
    gst = request(PORT, '/api/status?t=%s' % TOKEN)
    check('无黑窗版 /api/status 报 gui=true', gst and gst.get('gui') is True, gst.get('gui') if isinstance(gst, dict) else gst)
    check('无黑窗版 /api/status 可正常访问（没崩）', gst and gst.get('canDownload') is True, (gst or {}).get('canDownload'))
    p4.terminate()

    # ---------- ③ 网页控制台接口已彻底移除（防回归，应一律 404）----------
    print('== C. 网页控制台接口已移除（不再通过网页暴露本地信息）==')
    p5, _ = start(CON_EXE, PORT, [MAT_A], 'srv_auth.log')
    c = code_of(PORT, '/api/console')
    check('/api/console 已移除（404）', c == 404, c)
    ol = code_of(PORT, '/api/open-log', method='POST')
    check('/api/open-log 已移除（404）', ol == 404, ol)
    sd = code_of(PORT, '/api/shutdown', method='POST')
    check('/api/shutdown 已移除（404）', sd == 404, sd)
    ok = code_of(PORT, '/api/status')
    check('普通接口 /api/status 仍正常（200）', ok == 200, ok)
    p5.terminate()

    # ---------- ④ 源码 / 资源断言（防改回去）----------
    print('== D. 源码与产物断言 ==')
    for fn, want in [('mode_console.go', 'buildMode = "console"'),
                     ('mode_gui.go', 'buildMode = "gui"')]:
        fp = os.path.join(D, fn)
        ok = os.path.exists(fp) and want in open(fp, encoding='utf-8').read()
        check('存在 %s 且声明 %s' % (fn, want), ok)
    # 本地桌面窗口控制台实现（纯 Win32 版，替代了会静默卡死的 walk 库）
    gw = os.path.join(D, 'gui_win32.go')
    gs = open(gw, encoding='utf-8').read() if os.path.exists(gw) else ''
    check('gui_win32.go 实现 runGUI（纯 Win32 本地窗口）', 'func runGUI(' in gs and 'user32.dll' in gs, '' if gs else 'missing')
    check('已弃用 walk 库（不再是/不再是构建依赖）', not os.path.exists(os.path.join(D, 'gui_walk.go')))
    gstub = os.path.join(D, 'gui_stub.go')
    gss = open(gstub, encoding='utf-8').read() if os.path.exists(gstub) else ''
    check('gui_stub.go 提供非 gui 构建的空 runGUI', 'func runGUI(' in gss, '' if gss else 'missing')
    # 网页控制台接口 / 前端按钮 已移除
    api_go = open(os.path.join(D, 'api.go'), encoding='utf-8').read()
    check('api.go 不再注册网页控制台路由',
          'GET /api/console' not in api_go and 'POST /api/shutdown' not in api_go and 'POST /api/open-log' not in api_go)
    main_go = open(os.path.join(D, 'main.go'), encoding='utf-8').read()
    check('main.go 用 flag.Args() 接位置参数', 'flag.Args()' in main_go and '拖' in main_go)
    check('main.go 日志落盘（setupLogging）', 'func setupLogging' in main_go)
    # 需求变更（2026-09-25）：ui 版**不再**默认自动开浏览器 ——
    # 双击只弹本地窗口（先是纯 Win32，现在是 WebView2 网页级窗口），
    # 想用浏览器就从窗口里点「打开下载页/审阅页」，或显式传 -open。
    check('ui 版不再默认自动开浏览器', 'flagWasSet("open")' not in main_go and '*autoOpen = true' not in main_go)
    check('-open 仍能手动打开浏览器', '*autoOpen' in main_go and 'openBrowser(' in main_go)
    # wails 版本地窗口（WebView2，网页级界面）
    gwails = os.path.join(D, 'gui_wails.go')
    gws = open(gwails, encoding='utf-8').read() if os.path.exists(gwails) else ''
    ok = ('func runWails(' in gws and 'wails.Run(' in gws and 'AssetServer' in gws) if gws else False
    check('gui_wails.go 用 WebView2 起本地窗口', ok, '' if gws else 'missing')
    check('存在 mode_wails.go 且声明 buildMode = "wails"',
          os.path.exists(os.path.join(D, 'mode_wails.go')) and 'buildMode = "wails"' in open(os.path.join(D, 'mode_wails.go'), encoding='utf-8').read())
    # 窗口里三个可输入项：素材根目录 / 监听地址 / 下载口令
    hitml = os.path.join(D, 'ui', 'index.html')
    hs = open(hitml, encoding='utf-8').read() if os.path.exists(hitml) else ''
    ok = ('rootInput' in hs and 'addrInput' in hs and 'tokenInput' in hs) if hs else False
    check('ui/index.html 里三个配置都能输入（根目录/地址/口令）', ok, '' if hs else 'missing')
    ok = ('window.go.main.UI.Logs' in hs or 'Logs(' in hs) if hs else False
    check('本地窗口读的是进程内日志（不经 HTTP）', ok)
    # 运行时改配置的能力：这三个入口必须活着
    ag = open(os.path.join(D, 'app.go'), encoding='utf-8').read() if os.path.exists(os.path.join(D, 'app.go')) else ''
    ok = ('func (a *app) SetRoot(' in ag and 'func (a *app) SetToken(' in ag and 'func (a *app) Rebind(' in ag) if ag else False
    check('app.go 支持运行时改根目录/口令/地址（不用重启）', ok, '' if ag else 'missing')
    check('两份 exe 都已构建', os.path.exists(CON_EXE) and os.path.exists(GUI_EXE),
          '%s / %s' % (os.path.exists(CON_EXE), os.path.exists(GUI_EXE)))
    # 前端：无网页控制台
    for html in ('web/review.html', 'web/download.html'):
        ok = 'id="btn-console"' not in open(os.path.join(D, html), encoding='utf-8').read()
        check('%s 已无网页控制台按钮' % html, ok)
    app_js = open(os.path.join(D, 'web/app.js'), encoding='utf-8').read()
    check('app.js 不再有 openConsole / /api/console?since=', 'openConsole' not in app_js and '/api/console?since=' not in app_js)
    # 快捷方式存在
    for lnk in ('medreview_ui.lnk', 'medreview.lnk'):
        check('存在快捷方式 %s' % lnk, os.path.exists(os.path.join(D, lnk)))

    # ---------- 汇总 ----------
    print()
    npass = sum(1 for _, ok, _ in results if ok)
    nfail = len(results) - npass
    print('=== 结果：%d PASS / %d FAIL ===' % (npass, nfail))
    with open(os.path.join(D, '_verify', 'result_startmodes.txt'), 'w', encoding='utf-8') as f:
        for name, ok, detail in results:
            f.write('[%s] %s%s\n' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    raise SystemExit(1 if nfail else 0)


if __name__ == '__main__':
    try:
        main()
    finally:
        for p in PROCS:
            if p.poll() is None:
                p.terminate()
