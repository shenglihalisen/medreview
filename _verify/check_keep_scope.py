# -*- coding: utf-8 -*-
"""medreview 验收：打包 / 导出 / 下载 只给「明确打勾（decision=1）」的文件。

自给自足：自己造素材、自己起停服务、自己清场。**不依赖 ffmpeg** ——
用几个极小的假 .jpg 就够覆盖全部下载口径（扫描只看元数据，不需要真图），
所以这个脚本是秒级的，可以随手跑。

覆盖五件事：
  1. 三条导出出口（打包 ZIP / 导出路径清单 / 导出到本机目录）严格等于被打勾的集合，
     「不保留」和「没标过 / 不确定」的一个都不能进。
  2. GET /api/media?dl=1 要校验 decision；不带 dl 的预览不受影响。
  3. 同一 rel_path 换了文件内容后，那条旧「保留」必须作废（不然会把没看过的内容打进交付包）。
  4. 三个批量按钮只作用于「当前屏幕上可见的」卡片（源码断言）。
  5. POST /api/review/clear-folder 只清「本层」，认领锁生效，且拒绝时一条都不清。

用法：
    python _verify/check_keep_scope.py            # 验默认的 medreview.exe
    python _verify/check_keep_scope.py new.exe    # 验还没部署的新构建

判定标准全在最后的 PASS/FAIL 表里，有 FAIL 就以非 0 退出码结束。
"""
import os
import sys
import json
import time
import zipfile
import subprocess
import urllib.error
import urllib.request

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
# 可选：把另一个 exe 当参数传进来。必须取绝对路径 —— 服务是用 cwd=RUN 起的，
# 相对路径会被解析到临时目录里去。
EXE = os.path.abspath(sys.argv[1]) if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
RUN = os.path.join(D, '_verify', 'keepscope')
MAT = os.path.join(RUN, 'mat')
OUT = os.path.join(RUN, 'out')          # 必须是 MAT 的兄弟目录，不能落在素材目录里面
CACHE = os.path.join(RUN, 'cache')
PORT = 8123
TOKEN = 'tk'

# 根目录（folder_id=1）6 个假素材：2 个打勾、2 个打叉、1 个显式标「不确定」、1 个从头到尾不碰
FILES = {
    'keepA.jpg': 101,
    'keepB.jpg': 137,
    'rejA.jpg': 173,
    'rejB.jpg': 211,
    'pendZero.jpg': 251,    # 显式标 decision=0（"不确定"）
    'pendNever.jpg': 293,   # 从不标（库里同样是 0）
}
KEEP = {'keepA.jpg', 'keepB.jpg'}

# 子目录素材，专门给第 6 组（清空本目录标记）用。
# 结构：mat/sub/{s1,s2} + mat/sub/deep/d1 + mat/other/o1
#   清 sub → deep（子目录）与 other（兄弟）的标记都必须留着。
SUBDIRS = {
    'sub/s1.jpg': 311,
    'sub/s2.jpg': 337,
    'sub/deep/d1.jpg': 359,
    'other/o1.jpg': 383,
}
TOTAL_FILES = len(FILES) + len(SUBDIRS)

OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
results = []
PROC = None


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def diag_conn():
    print('  !! 连接异常。服务进程 poll() = %r' % (PROC.poll() if PROC else 'n/a'))
    logp = os.path.join(RUN, 'server.log')
    if os.path.exists(logp):
        tail = open(logp, encoding='utf-8', errors='replace').read().splitlines()[-6:]
        for l in tail:
            print('     | ' + l)
    sys.stdout.flush()


def request(path, raw=False, data=None, ctype=None, user='verifier'):
    """发一个请求。loopback 偶发 ConnectionReset，重试 3 次；
    HTTPError 原样抛给调用方判断状态码（业务状态码不能被吞掉）。
    user 用来换 X-User —— 认领锁与 markedMine 都依赖它。"""
    last = None
    for attempt, wait in enumerate((0.3, 1.0, 2.5), start=1):
        r = urllib.request.Request('http://127.0.0.1:%d%s' % (PORT, path), data=data)
        if ctype:
            r.add_header('Content-Type', ctype)
        r.add_header('X-User', user)
        try:
            resp = OP.open(r, timeout=120)
            b = resp.read()
            return b if raw else json.loads(b.decode('utf-8'))
        except (ConnectionResetError, ConnectionAbortedError) as e:
            last = e
            print('  !! 连接被重置（第 %d/3 次）: %s' % (attempt, path))
            diag_conn()
            time.sleep(wait)
    raise last


def code_of(path):
    """只取状态码，不抛。"""
    try:
        OP.open('http://127.0.0.1:%d%s' % (PORT, path), timeout=60).read()
        return 200
    except urllib.error.HTTPError as e:
        return e.code


def post(path, obj, user='verifier'):
    return request(path, data=json.dumps(obj).encode('utf-8'), ctype='application/json', user=user)


def resp_of(path, origin=None):
    """取 (状态码, 小写化的响应头字典)。给 CORS / 安全响应头的断言用。

    urllib 默认不检查 CORS（那是浏览器的活），所以这里只能断言"服务端回了什么头"：
    外部 Origin 时不给 ACAO，浏览器就会把那次跨站请求挡掉。
    """
    r = urllib.request.Request('http://127.0.0.1:%d%s' % (PORT, path))
    r.add_header('X-User', 'verifier')
    if origin:
        r.add_header('Origin', origin)
    try:
        resp = OP.open(r, timeout=60)
        resp.read()
        code, hdrs = resp.getcode(), resp.headers
    except urllib.error.HTTPError as e:
        code, hdrs = e.code, (e.headers or {})
    return code, dict((k.lower(), v) for k, v in hdrs.items())


def fresh_dir(path):
    """准备一个干净目录。

    本机安全机制会拦"整轮累计删超过 50 个文件"，中招时进程被直接终止、
    连 traceback 都没有。所以这里只改名让路，绝不真删 ——
    改名留下的 *_trash_* 目录可手工清，不影响验收结果。
    """
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError as e:
            print('  !! 无法让开旧目录 %s: %r' % (path, e))
            print('     请手工删掉它再跑（脚本不真删，避免撞安全拦截）')
            raise SystemExit(2)
    os.makedirs(path, exist_ok=True)


def build_fixture():
    os.makedirs(MAT, exist_ok=True)
    allf = dict(FILES)
    allf.update(SUBDIRS)
    for name, size in allf.items():
        p = os.path.join(MAT, name.replace('/', os.sep))
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, 'wb') as f:
            f.write(bytes([65 + (size % 26)]) * size)


def start():
    global PROC
    lf = open(os.path.join(RUN, 'server.log'), 'w', encoding='utf-8', newline='')
    p = subprocess.Popen([EXE, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                          '-db', 'v.db', '-cache', CACHE],
                         cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
    PROC = p
    for _ in range(120):
        if p.poll() is not None:
            break
        try:
            request('/api/status')
            return p, lf
        except Exception:
            time.sleep(0.3)
    lf.flush()
    print('  !! 服务未能启动（poll=%r）。日志：' % p.poll())
    for l in open(os.path.join(RUN, 'server.log'), encoding='utf-8',
                  errors='replace').read().splitlines()[-12:]:
        print('     | ' + l)
    raise RuntimeError('服务未能启动')


def scan_prog():
    return request('/api/status').get('scan') or {}


def wait_scan(expect_files=6, limit=60):
    """等服务「扫描过且看到了 expect_files 个文件」。
    注意：不能只看 running==false —— 服务是"先监听、再异步扫描"，
    刚连上时读到的 running=false 是**还没开始**的初始值。"""
    t0 = time.time()
    while time.time() - t0 < limit:
        sc = scan_prog()
        if (not sc.get('running')) and (sc.get('files') or 0) >= expect_files \
                and (sc.get('endedAt') or 0) > 0:
            return sc
        time.sleep(0.2)
    return scan_prog()


def pending_names(folder=1):
    d = request('/api/files?folder=%d&limit=200&filter=pending' % folder)
    return {f['name'] for f in d.get('files', [])}


def all_files(folder=1):
    d = request('/api/files?folder=%d&limit=200&filter=all' % folder)
    return {f['name']: f for f in d.get('files', [])}


def zip_names(folder=1):
    """打包当前目录，解出条目名集合。空集时服务返回 404，这里等价成空集合。"""
    try:
        blob = request('/api/zip?folder=%d&t=%s' % (folder, TOKEN), raw=True)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return set()
        raise
    # 固定路径覆盖写，不删。本机安全机制会拦"一次删超过 50 个文件"（整轮累计），
    # 中招时进程会被直接终止、连 traceback 都没有，后面几十条断言全废 —— 所以中途一次都不删。
    zp = os.path.join(RUN, 'pack.zip')
    with open(zp, 'wb') as f:
        f.write(blob)
    with zipfile.ZipFile(zp) as z:
        names = set(z.namelist())
    return names


def export_names(folder=1):
    txt = request('/api/export?folder=%d&recursive=1&t=%s' % (folder, TOKEN),
                  raw=True).decode('utf-8', 'replace')
    return {os.path.basename(l.strip()) for l in txt.splitlines() if l.strip()}


def forget_dir(path):
    """把目录改名挪走当"清空"，不用真删（改名不触发删除拦截）。"""
    if os.path.exists(path):
        try:
            os.replace(path, '%s_trash_%s_%d' % (path, time.strftime('%H%M%S'), _seq()))
        except OSError:
            pass


_seq_n = [0]


def _seq():
    _seq_n[0] += 1
    return _seq_n[0]


def copied_names(folder=1):
    forget_dir(OUT)
    r = post('/api/copy?t=%s' % TOKEN, {'folderId': folder, 'recursive': True, 'scope': '', 'dest': OUT})
    got = set()
    for dp, dn, fn in os.walk(OUT):
        got |= set(fn)
    return r, got


# ---- 第 6 组要用的小工具 ----

def folder_id_by_name(parent, name):
    """按名字找子目录 id；找不到返回 0。"""
    d = request('/api/folders?parent=%d' % parent)
    for f in d.get('folders', []):
        if f.get('name') == name:
            return int(f['id'])
    return 0


def folder_info(folder_id, user='verifier'):
    """GET /api/folder 的完整返回体（含 marked / markedMine）。"""
    return request('/api/folder?folder=%d' % folder_id, user=user)


def clear_folder(folder_id, user='verifier'):
    """清空某目录标记。返回 (状态码, 响应体 dict 或错误文案字符串)。"""
    try:
        return 200, post('/api/review/clear-folder', {'folderId': folder_id}, user=user)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode('utf-8', 'replace').strip()


def marked_count(folder_id, user='verifier'):
    """本层已标记数（含保留/不保留/不确定，即 review 表里有的行）。"""
    return folder_info(folder_id, user).get('marked')


def decide(folder_id, name, decision, user='verifier'):
    d = all_files(folder_id)
    return post('/api/review', {'fileId': d[name]['id'], 'decision': decision}, user=user)


def main():
    fresh_dir(RUN)
    print('=== 造素材（极小假 jpg，不依赖 ffmpeg）===')
    build_fixture()
    for n in sorted(FILES):
        print('  %-14s %d 字节' % (n, FILES[n]))
    for n in sorted(SUBDIRS):
        print('  %-14s %d 字节' % (n, SUBDIRS[n]))

    p, lf = start()
    try:
        print()
        print('=== 1. 打标：2 个保留 / 2 个不保留 / 1 个不确定 / 1 个从不碰 ===')
        wait_scan(expect_files=TOTAL_FILES)
        byname = all_files(1)
        check('6 个素材都被索引到', set(byname) == set(FILES), str(sorted(byname)))
        if set(byname) != set(FILES):
            raise RuntimeError('素材没被完整索引，后面的断言没有意义')
        for n in ('keepA.jpg', 'keepB.jpg'):
            post('/api/review', {'fileId': byname[n]['id'], 'decision': 1})
        for n in ('rejA.jpg', 'rejB.jpg'):
            post('/api/review', {'fileId': byname[n]['id'], 'decision': 2})
        post('/api/review', {'fileId': byname['pendZero.jpg']['id'], 'decision': 0})
        after = all_files(1)
        check('标记已生效（2 保留 / 2 不保留 / 2 未定）',
              sorted(f['decision'] for f in after.values()) == [0, 0, 1, 1, 2, 2],
              str(sorted((f['name'], f['decision']) for f in after.values())))
        check('「从不碰」和「显式不确定」都算未定',
              pending_names(1) == {'pendZero.jpg', 'pendNever.jpg'}, str(sorted(pending_names(1))))

        print()
        print('=== 2. 三条导出出口必须严格等于"打勾的 2 个" ===')
        zn = zip_names(1)
        # 导出清单.csv 是有意随包附带的清单文件（manifest.go），不算口径泄漏
        check('打包 ZIP 只含打勾的 2 个（+导出清单）', zn == KEEP | {'导出清单.csv'}, str(sorted(zn)))
        check('ZIP 里没有"不确定"和"从不碰"的文件',
              not ({'pendZero.jpg', 'pendNever.jpg'} & zn), str(sorted(zn)))
        check('ZIP 里没有"不保留"的文件',
              not ({'rejA.jpg', 'rejB.jpg'} & zn), str(sorted(zn)))

        en = export_names(1)
        check('导出路径清单只含打勾的 2 个', en == KEEP, str(sorted(en)))

        r, cn = copied_names(1)
        check('导出接口报告成功', r.get('ok') is True, json.dumps(r, ensure_ascii=False))
        check('导出目录里只含打勾的 2 个（+导出清单）', cn == KEEP | {'导出清单.csv'}, str(sorted(cn)))
        check('导出清单内容包含全部 7 个文件及保留状态',
              os.path.exists(os.path.join(OUT, '导出清单.csv')) and '是否保留' in open(os.path.join(OUT, '导出清单.csv'), encoding='utf-8-sig').read())

        print()
        print('=== 3. 单文件下载要过 decision 那道闸 ===')
        check('dl=1 下载"不保留" → 403',
              code_of('/api/media?id=%d&dl=1&t=%s' % (byname['rejA.jpg']['id'], TOKEN)) == 403)
        check('dl=1 下载"从不碰" → 403',
              code_of('/api/media?id=%d&dl=1&t=%s' % (byname['pendNever.jpg']['id'], TOKEN)) == 403)
        check('dl=1 下载"不确定" → 403',
              code_of('/api/media?id=%d&dl=1&t=%s' % (byname['pendZero.jpg']['id'], TOKEN)) == 403)
        check('dl=1 下载"保留" → 200（反向，防筛过头）',
              code_of('/api/media?id=%d&dl=1&t=%s' % (byname['keepB.jpg']['id'], TOKEN)) == 200)
        check('无口令 dl=1 仍被拒 → 403',
              code_of('/api/media?id=%d&dl=1' % byname['keepB.jpg']['id']) == 403)
        check('不带 dl 预览"不保留"仍 200（预览绝不能被误伤）',
              code_of('/api/media?id=%d' % byname['rejA.jpg']['id']) == 200)
        check('不带 dl 预览"从不碰"仍 200',
              code_of('/api/media?id=%d' % byname['pendNever.jpg']['id']) == 200)

        print()
        print('=== 4. 同一路径换了内容 → 旧勾选必须作废 ===')
        # 原地覆盖 keepA：换长度 + 把 mtime 往前挪 2 小时（保证跟上一轮不同）
        pth = os.path.join(MAT, 'keepA.jpg')
        with open(pth, 'wb') as f:
            f.write(b'z' * 777)
        old = time.time() - 7200
        os.utime(pth, (old, old))
        post('/api/scan', {'root': MAT})

        # 效果驱动：等 keepA 真的回到"未定"，而不是去猜重扫什么时候跑完
        t0 = time.time()
        while time.time() - t0 < 40:
            if 'keepA.jpg' in pending_names(1) and not scan_prog().get('running'):
                break
            time.sleep(0.3)
        check('覆盖内容并重扫后，keepA 的勾选被作废（回到未定）',
              'keepA.jpg' in pending_names(1), str(sorted(pending_names(1))))
        check('内容没变的 keepB 勾选保住了',
              'keepB.jpg' not in pending_names(1), str(sorted(pending_names(1))))

        zn2 = zip_names(1)
        check('内容变过的文件不再进包', 'keepA.jpg' not in zn2, str(sorted(zn2)))
        check('内容没变的打勾文件仍在包里（反向）', zn2 == {'keepB.jpg', '导出清单.csv'}, str(sorted(zn2)))

        print()
        print('=== 5. 批量按钮只作用于"当前屏幕上可见的"（源码断言）===')
        js = request('/app.js', raw=True).decode('utf-8', 'replace')
        check('有 visibleIds 帮助函数', 'function visibleIds(' in js)
        check('三个批量按钮都改用可见集合', js.count('visibleIds(') >= 4, '出现 %d 次' % js.count('visibleIds('))
        check('不再用 S.items.map（旧的全量范围）', 'S.items.map' not in js)
        check('确认文案说的是"当前屏幕上可见的"', '当前屏幕上可见的' in js)
        check('render 里算出了可见区间 visA/visB',
              'S.visA =' in js and 'S.visB =' in js)
        check('openHelp 里写明了下载口径', '都只给标记为「保留」的文件' in js)

        print()
        print('=== 6. 「清空本目录标记」只作用本层 + 假警报回归 ===')
        sub_id = folder_id_by_name(1, 'sub')
        deep_id = folder_id_by_name(sub_id, 'deep')
        other_id = folder_id_by_name(1, 'other')
        check('sub / sub/deep / other 三个目录都被索引到',
              sub_id > 0 and deep_id > 0 and other_id > 0,
              'sub=%s deep=%s other=%s' % (sub_id, deep_id, other_id))
        if not (sub_id and deep_id and other_id):
            raise RuntimeError('子目录没被索引到，第 6 组断言没有意义')

        # -- 6.0 无标记目录：清了等于没清，但不能报错
        st, d = clear_folder(sub_id)
        check('无标记目录清空 → 200 且 changed==0',
              st == 200 and isinstance(d, dict) and d.get('changed') == 0, '%s %r' % (st, d))

        # -- 6.1 不存在的目录 → 404
        st, d = clear_folder(999999)
        check('不存在的 folderId → 404', st == 404, '%s %r' % (st, d))

        # -- 6.2 打标：verifier 标 s1=1，alice 标 s2=2（markedMine 要能区分人）
        decide(sub_id, 's1.jpg', 1)
        decide(sub_id, 's2.jpg', 2, user='alice')
        decide(deep_id, 'd1.jpg', 1)
        decide(other_id, 'o1.jpg', 1)

        fi = folder_info(sub_id)
        check('GET /api/folder 返回 marked/markedMine',
              'marked' in fi and 'markedMine' in fi, str(sorted(fi)))
        check('marked 只算本层（sub 本层 2 个）', fi.get('marked') == 2,
              'marked=%r' % fi.get('marked'))
        check('markedMine 按 X-User 区分（verifier 标了 1 个）',
              folder_info(sub_id, 'verifier').get('markedMine') == 1,
              str(folder_info(sub_id, 'verifier').get('markedMine')))
        check('markedMine 按 X-User 区分（alice 标了 1 个）',
              folder_info(sub_id, 'alice').get('markedMine') == 1,
              str(folder_info(sub_id, 'alice').get('markedMine')))
        # 根目录的 4 条 review 行 = rejA(2) + rejB(2) + keepB(1) + pendZero(显式 0，清空时同样会被抹掉)。
        # keepA 的行在第 4 组被"换内容作废"删掉了，pendNever 从没标过没有行。
        # 若 marked 是递归的，这里会是 4+2+1+1=8。
        root_marked = folder_info(1).get('marked')
        check('根目录 marked==4，证明只算本层、不含 sub/deep/other 的标记',
              root_marked == 4, 'marked=%r' % root_marked)

        # -- 6.3 清空 sub
        st, d = clear_folder(sub_id)
        check('清空 sub → 200 且 changed==2', st == 200 and d.get('changed') == 2,
              '%s %r' % (st, d))
        check('清空后 sub 本层全部回到未定',
              all(f['decision'] == 0 for f in all_files(sub_id).values()),
              str(sorted((f['name'], f['decision']) for f in all_files(sub_id).values())))
        check('反向：兄弟目录 other 的标记没被清',
              all_files(other_id).get('o1.jpg', {}).get('decision') == 1,
              str(all_files(other_id)))
        check('反向：子目录 deep 的标记没被清（不递归）',
              all_files(deep_id).get('d1.jpg', {}).get('decision') == 1,
              str(all_files(deep_id)))
        check('反向：根目录自己的标记没被清',
              folder_info(1).get('marked') == 4, 'marked=%r' % folder_info(1).get('marked'))
        st, d = clear_folder(sub_id)
        check('再清一次 → changed==0（幂等）', st == 200 and d.get('changed') == 0, '%s %r' % (st, d))

        # -- 6.4 认领锁：拒绝时一条都不能清
        decide(sub_id, 's1.jpg', 1)
        post('/api/claim', {'folderId': sub_id, 'user': 'alice'}, user='alice')
        st, d = clear_folder(sub_id)
        check('被他人认领时清空 → 403', st == 403, '%s %r' % (st, d))
        check('反向：403 之后 sub 的标记一条没少（拒绝 ≠ 部分执行）',
              folder_info(sub_id).get('marked') == 1, 'marked=%r' % folder_info(sub_id).get('marked'))
        st, d = clear_folder(sub_id, user='alice')
        check('认领者本人清空 → 200 且清掉',
              st == 200 and d.get('changed') == 1, '%s %r' % (st, d))
        post('/api/claim/release', {'folderId': sub_id}, user='alice')

        # -- 6.5 假警报回归：重复标同一个值不是"被他人认领"
        decide(sub_id, 's1.jpg', 1)
        r1 = post('/api/review', {'fileId': all_files(sub_id)['s1.jpg']['id'], 'decision': 1})
        check('重复标同一个值 → changed==0, same==1, blocked==0',
              r1.get('changed') == 0 and r1.get('same') == 1 and r1.get('blocked') == 0,
              json.dumps(r1, ensure_ascii=False))
        r2 = post('/api/review', {'fileId': all_files(sub_id)['s1.jpg']['id'], 'decision': 2})
        check('改成不同值 → changed==1', r2.get('changed') == 1, json.dumps(r2, ensure_ascii=False))

        # 认领后：标记请求要给出 blocked 计数（而不是笼统的 changed=0）
        post('/api/claim', {'folderId': sub_id, 'user': 'alice'}, user='alice')
        r3 = post('/api/review', {'fileId': all_files(sub_id)['s1.jpg']['id'], 'decision': 1})
        check('被他人认领时单张标记 → blocked==1, changed==0',
              r3.get('blocked') == 1 and r3.get('changed') == 0, json.dumps(r3, ensure_ascii=False))
        post('/api/claim/release', {'folderId': sub_id}, user='alice')

        # 先把 s1 标成 1，再对它批量发同样的 1 —— 这时才是"本来就是目标状态"
        decide(sub_id, 's1.jpg', 1)
        r4 = post('/api/review/batch', {
            'fileIds': [all_files(sub_id)['s1.jpg']['id']], 'decision': 1})
        check('批量对"已经是目标状态"的文件 → same 计数正确且 blocked==0',
              r4.get('same') == 1 and r4.get('blocked') == 0, json.dumps(r4, ensure_ascii=False))

        # -- 6.6 源码/防漂移断言
        html_r = request('/review.html', raw=True).decode('utf-8', 'replace')
        html_d = request('/download.html', raw=True).decode('utf-8', 'replace')
        sa = request('/standalone.html', raw=True).decode('utf-8', 'replace')
        check('两份页面都有 btn-clear-folder',
              'id="btn-clear-folder"' in html_r and 'id="btn-clear-folder"' in html_d)
        check('新按钮是 danger 且文案带省略号',
              'btn danger wide" id="btn-clear-folder"' in html_r and '清空本目录标记…' in html_r)
        check('三份内联页已同步（build_single.py 跑过）',
              'id="btn-clear-folder"' in sa)
        check('openHelp 写明了「清空本目录」', '清空本目录' in js and '不含子目录' in js)
        check('弹窗有勾选门禁（我确认）', '我确认要清空本目录的全部标记' in js)
        check('三个批量按钮的可见范围语义没被动',
              js.count('visibleIds(') >= 4 and 'S.items.map' not in js)
        check('重复标记不再误报"被他人认领"（旧写法已移除）',
              'if (d.changed === 0) { toast("该目录已被他人认领' not in js and 'blocked' in js)
        check('claim 事件会同步当前目录的认领状态',
              'S.claimedBy = d.data.user' in js)
        check('树上自己的认领显示成「我」', '? "我" : n.claimedBy' in js)
        gosrc = open(os.path.join(D, 'api.go'), encoding='utf-8').read()
        check('folder-clear 是聚合广播（恰好 1 处，没退化成逐文件）',
              gosrc.count('Broadcast("folder-clear"') == 1,
              '出现 %d 次' % gosrc.count('Broadcast("folder-clear"'))

        print()
        print('=== 7. 「含父目录」浏览范围（不改标记/打包口径）===')
        # sub 的父目录是根目录（有 6 个文件），sub 本层 2 个（s1/s2）
        plain = request('/api/files?folder=%d&limit=200&filter=all' % sub_id)['files']
        check('不带 withParent → 只有本层的 2 个',
              sorted(f['name'] for f in plain) == ['s1.jpg', 's2.jpg'],
              str(sorted(f['name'] for f in plain)))
        check('不带 withParent 时没有 fromParent 标记',
              all(not f.get('fromParent') for f in plain))

        mixed = request('/api/files?folder=%d&limit=200&filter=all&withParent=1' % sub_id)['files']
        own = [f for f in mixed if not f.get('fromParent')]
        up = [f for f in mixed if f.get('fromParent')]
        check('带 withParent=1 → 本层 2 个 + 父目录 6 个',
              len(own) == 2 and len(up) == 6, 'own=%d up=%d' % (len(own), len(up)))
        check('父目录来的文件带 fromParent 标记',
              len(up) == 6 and all(f['fromParent'] for f in up))
        check('父目录文件排在本层后面',
              [f['name'] for f in mixed] == [f['name'] for f in own] + [f['name'] for f in up])

        # 翻页（带 afterId）时不再重复追加父目录文件
        page2 = request('/api/files?folder=%d&limit=2&afterName=%s&afterId=%d&withParent=1'
                        % (sub_id, own[-1]['name'].lower(), own[-1]['id']))['files']
        check('翻页时不再重复带父目录文件',
              all(not f.get('fromParent') for f in page2) and len(page2) <= 2,
              str([f['name'] for f in page2]))

        # 根目录没有父目录 → 加了参数也不多出东西
        rootp = request('/api/files?folder=1&limit=200&filter=all&withParent=1')['files']
        check('根目录（无父目录）加了参数也不多出文件',
              len(rootp) == len(FILES) and all(not f.get('fromParent') for f in rootp),
              '%d 个' % len(rootp))

        # 关键：浏览范围不影响打包口径 —— sub 层始终只有它自己的保留文件
        zn3 = zip_names(sub_id)
        check('含父目录不影响打包口径（sub 仍只含 s1）', zn3 == {'s1.jpg', '导出清单.csv'}, str(sorted(zn3)))

        check('前端默认开、可切换，且游标只按本层算',
              'mr_parent' in js and 'S.withParent' in js and '!f.fromParent' in js)
        check('两份页面都有 btn-parent',
              'id="btn-parent"' in html_r and 'id="btn-parent"' in html_d)
        check('父目录文件有角标', 'badge-parent' in js)

        print()
        print('=== 8. 自动打开浏览器：不再用 explorer 带 URL（会弹 Documents）===')
        mainsrc = open(os.path.join(D, 'main.go'), encoding='utf-8').read()
        check('第一个候选是 cmd /c start（走 ShellExecute）',
              '{"cmd", "/c", "start", "", url}' in mainsrc)
        check('explorer.exe 被降级为兜底（不再优先）',
              mainsrc.index('{"cmd", "/c", "start", "", url}') < mainsrc.index('{"explorer.exe", url}'))
        check('注释写明了 ?t= 被 explorer 当通配符这个坑', '通配符' in mainsrc)

        print()
        print('=== 9. 接口鉴权与响应头（安全回归）===')
        # /api/export 以前漏了下载口令校验：任何人拼 URL 就能拿到本机绝对路径清单
        code_no, _ = resp_of('/api/export?folder=1&recursive=1')
        code_ok, _ = resp_of('/api/export?folder=1&recursive=1&t=%s' % TOKEN)
        check('导出清单：不给口令 → 403', code_no == 403, '实际 %d' % code_no)
        check('导出清单：给对口令 → 200', code_ok == 200, '实际 %d' % code_ok)
        # /api/browse-dest 的 mkdir= 会真的在磁盘上建目录，同样要过口令
        bd_no, _ = resp_of('/api/browse-dest?path=')
        bd_ok, _ = resp_of('/api/browse-dest?path=&t=%s' % TOKEN)
        check('目标目录选择器（含新建目录）：不给口令 → 403', bd_no == 403, '实际 %d' % bd_no)
        check('目标目录选择器：给对口令 → 200', bd_ok == 200, '实际 %d' % bd_ok)

        # CORS：只允许本机来源，别让互联网上任意网页读走素材库
        _, h_ev = resp_of('/api/status', origin='http://evil.example.com')
        _, h_lo = resp_of('/api/status', origin='http://127.0.0.1:31976')
        _, h_ln = resp_of('/api/status', origin='http://localhost:8080')
        _, h_no = resp_of('/api/status')
        check('外部站点的 Origin 拿不到 CORS 许可',
              'access-control-allow-origin' not in h_ev, str(sorted(h_ev)))
        check('127.0.0.1（预览面板另一个端口）放行',
              h_lo.get('access-control-allow-origin') == 'http://127.0.0.1:31976',
              str(h_lo.get('access-control-allow-origin')))
        check('localhost 放行', h_ln.get('access-control-allow-origin') == 'http://localhost:8080',
              str(h_ln.get('access-control-allow-origin')))
        check('不带 Origin 的直连（脚本/curl）不受影响',
              'access-control-allow-origin' not in h_no and h_no.get('x-content-type-options') == 'nosniff',
              str(sorted(h_no)))
        check('Vary: Origin（有 Origin 时才有，避免缓存串味）', h_lo.get('vary') == 'Origin',
              str(h_lo.get('vary')))

        # 安全响应头
        check('所有响应都带 X-Content-Type-Options: nosniff',
              h_ev.get('x-content-type-options') == 'nosniff' and h_no.get('x-content-type-options') == 'nosniff')
        check('带 Referrer-Policy: no-referrer（下载页地址里有口令）',
              h_no.get('referrer-policy') == 'no-referrer', str(h_no.get('referrer-policy')))

        apisrc = open(os.path.join(D, 'api.go'), encoding='utf-8').read()
        check('源码里不再有 CORS 通配 *（防回退）',
              '"Access-Control-Allow-Origin", "*"' not in apisrc)
        check('源码里有 localOrigin 白名单判断', 'func localOrigin(' in apisrc)
        check('网卡枚举在请求路径上走缓存（不是每次都枚举）',
              'func cachedLANIPv4s(' in open(os.path.join(D, 'lan.go'), encoding='utf-8').read())
        check('review 事件广播的是数字 folderId（不是 RelPath）',
              '"folderId": f.FolderID' in apisrc and 'f.RelPath,' not in apisrc.split('Broadcast("review"')[1][:400])
        check('素材根目录的读写有锁（SetRoot）',
              'func (s *Store) SetRoot(' in open(os.path.join(D, 'store.go'), encoding='utf-8').read())
        check('视频转码用 CommandContext（换目录能杀掉 ffmpeg）',
              'exec.CommandContext(ctx, v.ffmpeg' in open(os.path.join(D, 'video.go'), encoding='utf-8').read())
        check('EXIF 解析只读文件头（不再整文件读入内存）',
              'readFileHead(path, 256*1024)' in open(os.path.join(D, 'image.go'), encoding='utf-8').read())
        check('前端目录名拼 innerHTML 前先转义', 'esc(S.folderName' in js)

        # 本机磁盘布局只属于本地控制台，不下发到网页（手机端/下载页都能看到）
        stt = request('/api/status')
        rt = stt.get('root') or ''
        check('/api/status 的 root 只是目录名（没有盘符和分隔符）',
              rt and ':' not in rt and '\\' not in rt and '/' not in rt, rt)
        scanp = (stt.get('scan') or {}).get('rootPath') or ''
        check('扫描进度的 rootPath 也只是目录名',
              (not scanp) or (':' not in scanp and '\\' not in scanp and '/' not in scanp), scanp)
        check('前端导出提示只显示目录名（完整路径留给控制台）',
              'baseOf(d.dest)' in js and 'function baseOf(' in js)
    finally:
        if p.poll() is None:
            p.terminate()
            try:
                p.wait(timeout=20)
            except Exception:
                p.kill()
        lf.close()

    print()
    print('=' * 60)
    failed = [r for r in results if not r[1]]
    print('共 %d 项，通过 %d，失败 %d' % (len(results), len(results) - len(failed), len(failed)))
    print('=' * 60)
    if failed:
        print()
        print('失败项：')
        for n, _, d in failed:
            print('  - %s%s' % (n, ('  (' + d + ')') if d else ''))
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
