# -*- coding: utf-8 -*-
"""手机适配 + 认领链路 + 名字门禁 + 完成审阅的端到端验收。

沿用既有约定：自己造素材、自己起停服务、逐条 PASS/FAIL、有 FAIL 则非 0 退出、
loopback 偶发 ConnectionReset 自动重试、独立端口与独立 -db/-cache（不碰 medreview.db）。

覆盖：
  A 静态断言   —— 抓 /style.css /review.html /download.html /app.js /standalone.html 的文本
  B 判手机/电脑 —— 两份 HTML 的内联判定脚本必须逐字一致（防漂移）
  C 认领       —— 端到端：claim → 树与目录都能看到认领人 → release → 都消失 → 幂等
  D 历史用户名 —— 服务端进程内存：用过的名字出现，重启进程后清空
  E 局域网地址 —— urls.txt 的 LAN 行与 banner 提示；只绑回环时的提示
  F 其它       —— /demo.html 已移出 embed（404）、localStorage 全在 try 里、不再用缩略图

用法：python _verify/check_mobile_shell.py [exe路径]
"""
import http.client
import json
import os
import re
import shutil
import subprocess
import sys
import time

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = sys.argv[1] if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
RUN = os.path.join(D, '_verify', 'mobile')
MAT = os.path.join(RUN, 'mat')
CACHE = os.path.join(RUN, 'cache')
PORT = 8117
PORT2 = 8118
TOKEN = 'tk'
results = []
PROC = None
LATEST_LOG = ''


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def info(msg):
    print('  (信息) %s' % msg)
    sys.stdout.flush()


def fresh_dir(path):
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError:
            shutil.rmtree(path, ignore_errors=True)
    os.makedirs(path, exist_ok=True)


# ---------------------------------------------------------------- HTTP
def _once(port, method, path, body=None, user='verifier'):
    c = http.client.HTTPConnection('127.0.0.1', port, timeout=60)
    try:
        h = {}
        if user is not None:
            h['X-User'] = user
        data = None
        if body is not None:
            data = json.dumps(body).encode('utf-8')
            h['Content-Type'] = 'application/json'
        c.request(method, path, body=data, headers=h)
        r = c.getresponse()
        return r.status, r.read()
    finally:
        c.close()


def req(port, method, path, body=None, user='verifier'):
    """本机 loopback 偶发 ConnectionReset（一次大响应之后的首个请求最常见），重试 3 次。"""
    last = None
    for i in range(3):
        try:
            return _once(port, method, path, body, user)
        except Exception as e:
            last = e
            if i < 2:
                time.sleep(0.8)
    raise last


def jget(port, path, user='verifier'):
    st, b = req(port, 'GET', path, user=user)
    if st != 200:
        raise RuntimeError('GET %s -> %d' % (path, st))
    return json.loads(b.decode('utf-8'))


def text(port, path, user='verifier'):
    st, b = req(port, 'GET', path, user=user)
    return st, b.decode('utf-8', 'replace')


def logtext():
    return open(LATEST_LOG, encoding='utf-8', errors='replace').read() if os.path.exists(LATEST_LOG) else ''


# ---------------------------------------------------------------- 起停
def start(exe, extra, tag, addr=None):
    global PROC, LATEST_LOG
    if addr is None:
        addr = ':%d' % PORT
    LATEST_LOG = os.path.join(RUN, tag + '.log')
    lf = open(LATEST_LOG, 'w', encoding='utf-8', newline='')
    q = subprocess.Popen([exe, '-root', MAT, '-addr', addr, '-token', TOKEN,
                          '-db', 'v.db', '-cache', CACHE] + extra,
                         cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
    PROC = q
    up = False
    for _ in range(160):
        if q.poll() is not None:
            break
        try:
            jget(port_of(addr), '/api/status')
            up = True
            break
        except Exception:
            time.sleep(0.3)
    if not up:
        print('  !! %s 启动失败 poll=%r，日志尾部：' % (tag, q.poll()))
        lf.flush()
        for l in open(LATEST_LOG, encoding='utf-8', errors='replace').read().splitlines()[-12:]:
            print('     | ' + l)
    return q, lf


def port_of(addr):
    return int(addr.rsplit(':', 1)[1])


def stop(q, lf):
    if q is None:
        return
    lf.flush()
    time.sleep(0.4)
    q.terminate()
    try:
        q.wait(timeout=25)
    except Exception:
        q.kill()
    lf.close()


def wait_scan(port=PORT, limit=90):
    t0 = time.time()
    while time.time() - t0 < limit:
        try:
            st = jget(port, '/api/status')
        except Exception:
            time.sleep(0.5)
            continue
        sc = st.get('scan') or {}
        if not sc.get('running'):
            return sc
        time.sleep(0.5)
    return {}


# ---------------------------------------------------------------- 素材
def seed_fixture():
    os.makedirs(MAT, exist_ok=True)
    sub = os.path.join(MAT, '2')
    os.makedirs(sub, exist_ok=True)
    # 3 张 .jpg（扫描只看扩展名/大小，内容不必是真 JPEG）
    for i, n in enumerate(['a_first.jpg', 'b_second.jpg', 'c_third.jpg']):
        with open(os.path.join(sub, n), 'wb') as f:
            f.write(b'\xff\xd8\xff\xe0' + b'x' * (2048 + i))
    # 根目录自身也放一个，顺带覆盖"根目录有文件"的情况
    with open(os.path.join(MAT, 'root_only.jpg'), 'wb') as f:
        f.write(b'\xff\xd8\xff\xe0' + b'y' * 1024)


# ---------------------------------------------------------------- 主体
def main():
    print('exe =', EXE)
    if not os.path.exists(EXE):
        print('找不到 exe: %s' % EXE)
        return 2
    fresh_dir(RUN)
    seed_fixture()

    q = lf = None
    try:
        q, lf = start(EXE, ['-vres', '720'], 'm0')
        wait_scan()

        # ============================================================ A. 静态
        print()
        print('=== A. 静态资源断言 ===')
        css_st, css = text(PORT, '/style.css')
        rv_st, rv = text(PORT, '/review.html')
        dl_st, dl = text(PORT, '/download.html')
        js_st, js = text(PORT, '/app.js')
        sa_st, sa = text(PORT, '/standalone.html')
        sd_st, sd = text(PORT, '/standalone_dl.html')
        check('核心静态资源都拿得到', css_st == 200 and rv_st == 200 and dl_st == 200
              and js_st == 200 and sa_st == 200 and sd_st == 200,
              'css=%d rv=%d dl=%d js=%d sa=%d sd=%d' % (css_st, rv_st, dl_st, js_st, sa_st, sd_st))

        # ============================================================ B. 判定
        print()
        print('=== B. 手机版/电脑版按浏览器特征判定 ===')
        BEGIN = '手机版/电脑版判定（BEGIN'
        END = '手机版/电脑版判定（END）'

        def judge_block(s):
            i = s.find(BEGIN)
            j = s.find(END)
            return s[i:j] if (i >= 0 and j > i) else ''

        brv, bdl = judge_block(rv), judge_block(dl)
        check('review.html 与 download.html 都含内联判定脚本', bool(brv) and bool(bdl))
        check('两份的判定脚本逐字一致（防漂移）', brv == bdl and len(brv) > 200,
              'len %d vs %d' % (len(brv), len(bdl)))
        for tok, label in [('userAgentData', 'userAgentData.mobile'),
                           ('pointer: coarse', '粗指针'),
                           ('hover: none', '无 hover'),
                           ('ontouchstart', '触屏检测'),
                           ('maxTouchPoints', '触点数'),
                           ('is-phone', '加 is-phone'),
                           ('is-pc', '加 is-pc'),
                           ('mr_ui', '记忆手动覆盖'),
                           ('"ui"', '?ui= 覆盖参数')]:
            check('判定脚本含 %s' % label, tok in brv)
        check('判定不用窗口宽度（没有 innerWidth 断点）', 'innerWidth' not in brv or 'portrait' in brv)
        check('CSS 含 html.is-phone 段', 'html.is-phone' in css)
        check('HTML 竖屏遮罩 #rotate-gate 存在', 'id="rotate-gate"' in rv and 'id="rotate-gate"' in dl)
        check('viewport 带 viewport-fit（给刘海机型留安全区）', 'viewport-fit=cover' in rv and 'viewport-fit=cover' in dl)
        check('CSS 使用 env(safe-area-inset', 'env(safe-area-inset' in css)

        # ============================================================ 手机布局
        print()
        print('=== C. 手机横屏布局 ===')
        check('侧栏手机下常驻变窄（html.is-phone #sidebar）', 'html.is-phone #sidebar' in css)
        check('确认没残留抽屉方案（translateX(-100%)）', 'translateX(-100%)' not in css)
        check('手机固定 1 列（JS 里 S.cols = 1）', re.search(r'S\.cols\s*=\s*1\s*;', js) is not None)
        check('触控按钮高度变量 --acks-h 存在且手机下是 44px',
              '--acks-h: 30px' in css and re.search(r'html\.is-phone\s*\{\s*--acks-h:\s*44px', css) is not None)
        check('卡片几何用 S.acksH 而不是硬编码 30',
              'S.acksH' in js and 'thumbH + 24 + S.acksH' in js and 'S.cellH - 24 - S.acksH' in js)
        check('工具栏「更多」收纳（#tb-more / #btn-more）',
              'id="tb-more"' in rv and 'id="btn-more"' in rv and '#tb-more { display: contents; }' in css)

        print()
        print('=== D. 灯箱左右分栏 + 按钮缩放 ===')
        for tok in ['lb-left', 'lb-right', 'lb-pager', 'lb-zin', 'lb-zout', 'lb-count']:
            check('灯箱含 %s' % tok, tok in rv and tok in dl)
        check('旧的 lb-nav 已彻底移除', 'lb-nav' not in rv and 'lb-nav' not in dl
              and 'lb-nav' not in css and 'lb-nav' not in js)
        check('旧的 lb-foot 已彻底移除', 'lb-foot' not in rv and 'lb-foot' not in css and 'lb-foot' not in js)
        check('缩放仅在放大后可拖动（clampPan + touch-action:none）',
              'clampPan' in js and 'touch-action: none' in css)
        check('拖动后吞掉补发的 click（防误关灯箱）',
              re.search(r'Z\.moved[\s\S]{0,200}stopPropagation\(\)', js) is not None)

        print()
        print('=== D2. 画质开关（原图/压缩图）===')
        check('两份页面都含 btn-quality', 'id="btn-quality"' in rv and 'id="btn-quality"' in dl)
        check('单文件版也带上了 btn-quality（build_single 已重跑）',
              'id="btn-quality"' in sa and 'id="btn-quality"' in sd)
        check('JS 有 mediaURL(f, orig)（统一走 orig=1 参数）', 'function mediaURL(' in js and '&orig=1' in js)
        check('JS 有 updateQualityBtn / defaultLbOrig',
              'function updateQualityBtn(' in js and 'function defaultLbOrig(' in js)
        check('画质偏好写 localStorage.mr_quality', 'mr_quality' in js)
        check('网格图片按画质开关取图', 'mediaURL(f, S.quality === "orig")' in js)
        check('灯箱按 S.lbOrig 取图', 'mediaURL(f, S.lbOrig)' in js)
        check('开灯箱 / 翻页都回到画质开关的默认值',
              js.count('defaultLbOrig()') >= 3, '出现 %d 次' % js.count('defaultLbOrig()'))
        check('切换画质会 reload（重新拉一遍）',
              re.search(r'btn-quality"\).onclick[\s\S]{0,400}?reload\(\)', js) is not None)
        check('帮助里写了画质开关（且说明下载永远原文件）',
              '画质开关' in js and '永远是原始文件' in js)

        print()
        print('=== D3. 滚轮 / 捏合缩放 + 放大后拖动 ===')
        check('连续缩放状态 Z = {s, tx, ty}（不再是离散档位）',
              re.search(r'var Z = \{ s: 1, tx: 0, ty: 0', js) is not None)
        check('缩放范围 100%~600%（ZMIN/ZMAX）',
              re.search(r'var ZMIN = 1, ZMAX = 6;', js) is not None)
        check('有锚点缩放 setScaleAt(s, ax, ay)', 'function setScaleAt(' in js)
        check('锚点坐标相对灯箱中心（anchorOf）', 'function anchorOf(' in js)
        check('zoomBy 支持传鼠标/手指坐标', 'function zoomBy(factor, clientX, clientY)' in js)
        check('滚轮监听带 passive:false（才能 preventDefault 不被滚动吞掉）',
              re.search(r'addEventListener\("wheel"[\s\S]{0,600}?passive: false', js) is not None)
        check('滚轮按 deltaMode 归一（行/页模式都换算）', 'e.deltaMode === 1' in js and 'e.deltaMode === 2' in js)
        check('双指捏合：touchstart 记 e.touches.length === 2',
              re.search(r'touchstart[\s\S]{0,600}?e\.touches\.length === 2', js) is not None)
        check('双指捏合：touchmove 用两指距离比算缩放',
              re.search(r'touchmove[\s\S]{0,900}?pinch\.dist', js) is not None)
        check('单指拖动只在放大后生效', 'zoomScale() <= 1 || e.touches.length !== 1' in js)
        check('鼠标拖动只在放大后生效',
              re.search(r'mousedown[\s\S]{0,300}?zoomScale\(\) <= 1', js) is not None)
        check('缩放同时作用于 img 和 video（lbMedia 取两者）',
              'querySelector("img, video")' in js)
        check('视频底部留 48px 给原生控制条（不然拖不动进度）', 'clientHeight - 48' in js)
        check('CSS 里视频有 transform-origin（不然缩放会以别处为中心）',
              re.search(r'\.lb-body video \{[\s\S]{0,300}?transform-origin', css) is not None)
        check('帮助里写了滚轮/捏合/拖动',
              '滚轮' in js and '双指捏合' in js and '拖动' in js)
        check('灯箱「看原图」按钮只对图片出现（updateOrigBtn 判 kind === 1）',
              'function updateOrigBtn(' in js and 'f.kind === 1' in js)

        # ============================================================ 认领
        print()
        print('=== E. 认领链路（端到端）===')
        folders0 = jget(PORT, '/api/folders?parent=0')['folders']
        fid = folders0[0]['id'] if folders0 else 0
        kids = jget(PORT, '/api/folders?parent=%d' % fid)['folders']
        sub_id = kids[0]['id'] if kids else fid
        info('根目录 id=%s，子目录 id=%s' % (fid, sub_id))

        st, _ = req(PORT, 'POST', '/api/claim', {'folderId': sub_id, 'user': 'alice'}, user='alice')
        check('POST /api/claim 返回 200', st == 200, 'status=%d' % st)
        row = [x for x in jget(PORT, '/api/folders?parent=%d' % fid)['folders'] if x['id'] == sub_id]
        check('树接口（/api/folders）里该节点 claimedBy = alice',
              bool(row) and row[0].get('claimedBy') == 'alice',
              json.dumps(row[0].get('claimedBy') if row else None, ensure_ascii=False))
        check('目录接口（/api/folder）里 claimedBy = alice',
              jget(PORT, '/api/folder?folder=%d' % sub_id)['folder'].get('claimedBy') == 'alice')
        check('服务端打出 [claim] 日志（下次真出问题可追溯）', '[claim]' in logtext(),
              ' | '.join(l for l in logtext().splitlines() if '[claim]' in l)[:150])

        st, _ = req(PORT, 'POST', '/api/claim', {'folderId': sub_id, 'user': 'alice'}, user='alice')
        check('重复认领是幂等的（仍 200，不会互相抵消）', st == 200, 'status=%d' % st)

        st, _ = req(PORT, 'POST', '/api/claim/release', {'folderId': sub_id}, user='alice')
        check('POST /api/claim/release 返回 200', st == 200, 'status=%d' % st)
        row = [x for x in jget(PORT, '/api/folders?parent=%d' % fid)['folders'] if x['id'] == sub_id]
        check('释放后树接口里 claimedBy 清空', bool(row) and not row[0].get('claimedBy'))
        check('释放后目录接口里 claimedBy 清空',
              not jget(PORT, '/api/folder?folder=%d' % sub_id)['folder'].get('claimedBy'))

        print()
        print('=== F. 认领前端加固（静态）===')
        check('JS 有防双击守卫（el.claimBtn.disabled）',
              'if (el.claimBtn.disabled) return;' in js)
        check('JS 有 applyOwnerLocal（认领后立刻更新左侧那一行）', 'applyOwnerLocal' in js)
        # claimBtn.onclick 这一段里必须有 .catch
        i = js.find('el.claimBtn.onclick')
        seg = js[i:i + 1400] if i >= 0 else ''
        check('claim/release 都有 .catch（失败不再静默）', seg.count('.catch(') >= 1 and '认领失败' in seg)
        check('updateClaimBtn 三个分支都复位 disabled',
              re.search(r'function updateClaimBtn\(\)[\s\S]{0,400}?el\.claimBtn\.disabled = false;', js) is not None)
        # treeRow 里 owner 必须插在 tc 之前
        tr = js[js.find('function treeRow('):js.find('function treeRow(') + 2600]
        check('树节点顺序：认领人（owner）在 keep/total 徽标（tc）之前',
              tr.find('"owner"') > 0 and 0 < tr.find('"owner"') < tr.find('"tc"'),
              'owner@%d tc@%d' % (tr.find('"owner"'), tr.find('"tc"')))
        check('CSS 里 .tree-row .owner 有底色便于辨认',
              re.search(r'\.tree-row \.owner \{[\s\S]{0,200}?background:', css) is not None)

        # ============================================================ 名字门禁
        print()
        print('=== G. 名字门禁（强制、不可跳过）===')
        for tok in ['id="name-gate"', 'id="name-input"', 'id="name-ok"', 'id="name-hist"']:
            check('HTML 含 %s' % tok, tok in rv and tok in dl)
        check('门禁没有「跳过」按钮', 'name-skip' not in rv and 'name-skip' not in js)
        for label, s in [('app.js', js), ('review.html', rv), ('download.html', dl),
                         ('standalone.html', sa), ('standalone_dl.html', sd), ('demo.html(生成源=review)', rv)]:
            check('%s 里不再有原生 prompt(' % label, 'prompt(' not in s)
        check('JS 有 guardName 与 userEntered', 'function guardName(' in js and 'userEntered' in js)
        for fn in ['function decide(', 'function decideMany(', 'function lbDecide(']:
            i = js.find(fn)
            body = js[i:i + 900] if i >= 0 else ''
            check('%s 走了 guardName' % fn.replace('function ', '').replace('(', ''), 'guardName(' in body)
        check('历史用户名下拉会去拉 /api/users', '/api/users' in js)
        check('#api/users 路由存在', jget(PORT, '/api/users') is not None)

        # ============================================================ 自动跳下一张 / 完成审阅
        print()
        print('=== H. 自动跳下一张 + 完成审阅 ===')
        i = js.find('function lbDecide(')
        body = js[i:i + 1000] if i >= 0 else ''
        check('灯箱标记成功后自动前进（lbDecide 里有 lbStep(1)）', 'lbStep(1)' in body)
        check('到了最后一张不循环、给提示', '已是最后一张' in body)
        check('键盘 0（不确定）也走同一套自动前进',
              re.search(r'if \(e\.key === "0"\) \{ lbDecide\(0\); return; \}', js) is not None)
        check('HTML 有「完成审阅」按钮', 'id="btn-finish"' in rv and 'id="btn-finish"' in dl)
        check('JS 用 filter=pending 找未定文件', 'filter=pending' in js)
        check('JS 支持翻页拉全量未定（afterName/afterId）',
              'afterName' in js and 'limit=300' in js)
        check('灯箱支持临时列表（lbList / openLightboxList / lbOverride）',
              'function lbList(' in js and 'function openLightboxList(' in js and 'lbOverride' in js)
        check('未定张数取自 folder.pending，按钮带角标', 'S.pending' in js and '完成审阅 · 还有' in js)

        # ============================================================ 其它加固
        print()
        print('=== I. 其它加固 ===')
        check('API_BASE 改为 file:/static-html 判定（修掉手机请求打到 127.0.0.1）',
              'location.protocol === "file:"' in js and 'static-html' in js)
        check('localStorage 全部包在 try 里（否则 init 中断、所有按钮失效）',
              all(('try {' in l and 'catch' in l) for l in js.splitlines() if 'localStorage.' in l),
              ' | '.join(l.strip() for l in js.splitlines() if 'localStorage.' in l and 'try {' not in l)[:160])
        check('前台返场重同步（visibilitychange / pageshow）',
              'visibilitychange' in js and 'pageshow' in js and 'function resync(' in js)
        check('图片一律走 /api/media 原图，不再请求缩略图',
              '/api/thumb' not in js and '/api/preview' not in js)
        check('视频卡片悬停预览（hoverPreview + 延迟补播 + 350ms 防误触）',
              'function hoverPreview(' in js and 'card._hoverWant' in js and '350' in js)
        check('窗口化渲染回收卡片时停掉悬停播放',
              'hoverPreview(node, false); node.remove()' in js and
              'hoverPreview(e, false); e.remove()' in js)
        check('buildStaticUI 幂等（列数/筛选控件不会被重复注入）',
              'S.uiBuilt' in js and 'sc.innerHTML = ""' in js)
        st, _ = text(PORT, '/demo.html')
        check('演示页已移出 embed（/demo.html 返回 404）', st == 404, 'status=%d' % st)
        check('三份内联页已同步新逻辑（standalone 里有门禁与 guardName）',
              'id="name-gate"' in sa and 'guardName' in sa and 'id="btn-finish"' in sa)

        # ============================================================ 局域网
        print()
        print('=== J. 局域网地址 ===')
        urls = open(os.path.join(RUN, 'urls.txt'), encoding='utf-8').read().splitlines()
        check('urls.txt 第 1 行是本机审阅页地址',
              bool(urls) and urls[0] == 'http://127.0.0.1:%d/review.html' % PORT,
              urls[0] if urls else '(空)')
        check('urls.txt 第 2 行是带口令的下载页',
              len(urls) > 1 and urls[1].startswith('http://127.0.0.1:%d/download.html?t=%s' % (PORT, TOKEN)),
              urls[1] if len(urls) > 1 else '(空)')
        lan = urls[2:]
        if lan:
            info('LAN 行 %d 条: %s' % (len(lan), ' , '.join(lan)))
            check('LAN 行里没有 169.254 链路本地地址',
                  not any('169.254.' in u for u in lan))
            check('LAN 行里没有 0.0.0.0',
                  not any('0.0.0.0' in u for u in lan))
            # 审阅页各一条；下载页只有一条（用的是第一个地址），所以去重只看审阅页那几条
            review_ips = [re.sub(r'^https?://([^:/]+).*$', r'\1', u)
                          for u in lan if u.endswith('/review.html')]
            check('LAN 地址已去重（审阅页那边没有重复 IP）',
                  len(review_ips) == len(set(review_ips)), str(review_ips))
            check('LAN 地址都是 RFC1918 私网地址',
                  all(re.match(r'^(10\.|172\.(1[6-9]|2\d|3[01])\.|192\.168\.)', ip) for ip in review_ips),
                  str(review_ips))
            check('LAN 里没有混进 127.0.0.1', not any(ip.startswith('127.') for ip in review_ips))
            dls = [u for u in lan if '/download.html' in u]
            check('LAN 下载页地址用的是第一个（默认出口）地址',
                  len(dls) == 1 and review_ips and ('://%s:' % review_ips[0]) in dls[0],
                  ' '.join(dls))
        else:
            info('本机没有可用的局域网地址，跳过 LAN 行内容校验')
        check('banner 给出了局域网提示（地址 或 未找到）',
              ('局域网' in logtext()) or ('未找到可用的局域网地址' in logtext()),
              ' | '.join(l for l in logtext().splitlines() if '局域网' in l)[:150])

        # 重启一次：验证历史用户名"关闭程序后失效"
        print()
        print('=== K. 历史用户名（服务端进程内存）===')
        req(PORT, 'POST', '/api/review',
            {'fileId': jget(PORT, '/api/files?folder=%d&limit=5' % sub_id)['files'][0]['id'],
             'decision': 1}, user='alice')
        users = jget(PORT, '/api/users')['users']
        check('用过的名字会被记住（/api/users 含 alice）', 'alice' in users, json.dumps(users, ensure_ascii=False))
        st, _ = req(PORT, 'POST', '/api/claim', {'folderId': sub_id, 'user': 'bob'}, user='bob')
        users = jget(PORT, '/api/users')['users']
        check('新用过的名字排在最近使用的前面', users and users[0] == 'bob', json.dumps(users, ensure_ascii=False))
        stop(q, lf)
        q, lf = start(EXE, ['-vres', '720'], 'm1')
        users = jget(PORT, '/api/users')['users']
        check('重启进程后名单清空（符合「关闭程序后失效」）', users == [], json.dumps(users, ensure_ascii=False))
        stop(q, lf)

        # 只绑回环时的提示
        print()
        print('=== L. 只绑回环时不报局域网地址 ===')
        q2, lf2 = start(EXE, ['-vres', '720'], 'm2', addr='127.0.0.1:%d' % PORT2)
        check('绑 127.0.0.1 时提示手机访问不到', '手机访问不到' in logtext(),
              ' | '.join(l for l in logtext().splitlines() if '手机访问不到' in l)[:150])
        urls2 = open(os.path.join(RUN, 'urls.txt'), encoding='utf-8').read().splitlines()
        check('绑回环时 urls.txt 只有 canonical 两行', len(urls2) == 2, '行数=%d' % len(urls2))
        check('绑回环时也不该枚举出别的网卡地址',
              not any(re.search(r'192\.168\.|10\.\d+\.|172\.\d+\.', u) for u in urls2),
              ' | '.join(urls2))
        stop(q2, lf2)
        q = lf = None

    finally:
        stop(q, lf)

    failed = [r for r in results if not r[1]]
    print()
    print('=' * 60)
    print('共 %d 项，通过 %d，失败 %d' % (len(results), len(results) - len(failed), len(failed)))
    for n, ok, d in failed:
        print('  FAIL: %s  %s' % (n, d))
    print('=' * 60)
    return 1 if failed else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception:
        import traceback
        traceback.print_exc()
        sys.exit(2)
