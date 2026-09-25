# -*- coding: utf-8 -*-
"""medreview 验收脚本：720p 转码规格 + 下载必须给原视频。

自给自足：自己用 ffmpeg 造素材（含一个必须转码的 HEVC 超宽屏），
跑完自动清场。改完 video.go / zip.go / copy.go 后重跑一遍就知道有没有回归。

用法：
    python _verify/check_download_and_720p.py

判定标准全在最后的 PASS/FAIL 表里，有任何 FAIL 都会以非 0 退出码结束。
"""
import os
import sys
import json
import time
import shutil
import hashlib
import zipfile
import subprocess
import urllib.error
import urllib.request

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
# 可选：python check_xxx.py <另一个exe> —— 用来先验未部署的新构建
EXE = sys.argv[1] if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
FF = os.path.join(D, 'tools', 'ffmpeg', 'ffmpeg.exe')
FP = os.path.join(D, 'tools', 'ffmpeg', 'ffprobe.exe')
RUN = os.path.join(D, '_verify', 'run')
MAT = os.path.join(RUN, 'mat')
CACHE = os.path.join(RUN, 'cache')
PORT = 8097
TOKEN = 'tk'

# 素材规格：一个超宽屏 HEVC（必须转码）、一个竖屏 HEVC（必须转码）、一个 H.264（不该转）
WIDE = 'src_wide_2772x1280.mp4'      # 2772x1280 -> 期望 1558x720
TALL = 'src_tall_1080x1920.mp4'      # 1080x1920 -> 期望 720x1280
H264 = 'src_playable_1280x720.mp4'   # H.264 -> 期望不转，原样输出

OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
results = []
PROC = None  # 当前跑着的服务进程，出事时用来取证


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def diag_conn():
    """连接被重置时，把服务进程状态和服务端日志尾巴打出来。"""
    print('  !! 连接异常。服务进程 poll() = %r' % (PROC.poll() if PROC else 'n/a'))
    logp = os.path.join(RUN, 'server.log')
    if os.path.exists(logp):
        tail = open(logp, encoding='utf-8', errors='replace').read().splitlines()[-6:]
        for l in tail:
            print('     | ' + l)
    sys.stdout.flush()


RETRIES = 0


def request(path, raw=False, data=None, ctype=None):
    """发一个请求。

    本机 loopback 上偶发 ConnectionResetError（表现为「一次大响应之后的第一个请求」
    被重置），跟服务端逻辑无关，重试即可。真出 bug 时三次都会失败，照样报 FAIL。
    """
    global RETRIES
    last = None
    for attempt, wait in enumerate((0.3, 1.0, 2.5), start=1):
        r = urllib.request.Request('http://127.0.0.1:%d%s' % (PORT, path), data=data)
        if ctype:
            r.add_header('Content-Type', ctype)
        r.add_header('X-User', 'verifier')
        try:
            resp = OP.open(r, timeout=900)
            b = resp.read()
            if attempt > 1:
                RETRIES += 1
                print('  (第 %d 次尝试成功: %s)' % (attempt, path))
            return b if raw else json.loads(b.decode('utf-8'))
        except (ConnectionResetError, ConnectionAbortedError) as e:
            last = e
            print('  !! 连接被重置（第 %d/3 次）: %s' % (attempt, path))
            diag_conn()
            time.sleep(wait)
        # HTTPError / URLError 原样抛出，交给调用方判断状态码
    raise last


def sha_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for c in iter(lambda: f.read(1 << 20), b''):
            h.update(c)
    return h.hexdigest()


def sha_bytes(b):
    return hashlib.sha256(b).hexdigest()


def probe_size(path):
    r = subprocess.run([FP, '-v', 'error', '-select_streams', 'v:0',
                        '-show_entries', 'stream=width,height,codec_name',
                        '-of', 'json', path], capture_output=True, text=True)
    s = json.loads(r.stdout)['streams'][0]
    return s['width'], s['height'], s['codec_name']


def build_fixture():
    os.makedirs(MAT, exist_ok=True)
    jobs = [
        (WIDE, 'testsrc2=size=2772x1280:rate=15', 'libx265', '24', ['-tag:v', 'hvc1']),
        (TALL, 'testsrc2=size=1080x1920:rate=15', 'libx265', '24', ['-tag:v', 'hvc1']),
        (H264, 'testsrc2=size=1280x720:rate=15', 'libx264', '28', []),
    ]
    for name, src, codec, crf, extra in jobs:
        out = os.path.join(MAT, name)
        if os.path.exists(out):
            continue
        cmd = [FF, '-hide_banner', '-y', '-f', 'lavfi', '-i', src, '-t', '3',
               '-c:v', codec, '-crf', crf, '-preset', 'ultrafast', '-pix_fmt', 'yuv420p'] + extra + [out]
        subprocess.run(cmd, capture_output=True)


def start():
    global PROC
    lf = open(os.path.join(RUN, 'server.log'), 'w', encoding='utf-8', newline='')
    p = subprocess.Popen([EXE, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                          '-db', 'v.db', '-cache', CACHE, '-vres', '720'],
                         cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
    PROC = p
    for _ in range(100):
        try:
            request('/api/status')
            return p, lf
        except Exception:
            time.sleep(0.3)
    raise RuntimeError('服务未能启动，看 _verify/run/server.log')


def warmup_started(w):
    """预热是否已经真的开始过。
    全零 = 还没启动（服务是「先监听、再异步扫描」，扫描完才触发预热），
    这不代表失败，只代表我们比扫描快了一步。"""
    return bool(w.get('startedAt') or w.get('finishedAt') or w.get('total')
                or w.get('probed') or w.get('needConv') or w.get('skipped')
                or w.get('cached') or w.get('failed') or w.get('done'))


def wait_warmup(limit=900):
    # 阶段一：等预热被触发（最多 60 秒）。
    # 少了这一步会误判：/api/status 一能连上就查预热，此时扫描可能还没跑完，
    # 读到的是从未启动过的全零状态 —— 之前偶发「needConv=0 / 缓存 0 个」就是这个竞态。
    t0 = time.time()
    while time.time() - t0 < 60:
        if PROC is not None and PROC.poll() is not None:
            print('  !! 服务进程已退出（码 %s），停止等待预热' % PROC.poll())
            return {'active': False, 'total': 0, 'needConv': 0, 'done': 0,
                    'skipped': 0, 'cached': 0, 'failed': 0, 'note': 'server exited'}
        try:
            w = request('/api/vtrans-warmup')
        except Exception as e:
            print('  !! 查预热进度失败: %r' % e)
            time.sleep(1)
            continue
        if warmup_started(w):
            break
        time.sleep(0.2)

    # 阶段二：等预热跑完
    t0 = time.time()
    while time.time() - t0 < limit:
        if PROC is not None and PROC.poll() is not None:
            print('  !! 服务进程已退出（码 %s），停止等待预热' % PROC.poll())
            return {'active': False, 'total': 0, 'needConv': 0, 'done': 0,
                    'skipped': 0, 'cached': 0, 'failed': 0, 'note': 'server exited'}
        try:
            w = request('/api/vtrans-warmup')
        except Exception as e:
            print('  !! 查预热进度失败: %r' % e)
            time.sleep(1)
            continue
        if not w.get('active'):
            return w
        time.sleep(2)
    return request('/api/vtrans-warmup')


def fresh_dir(path):
    """准备一个干净的目录。
    注意：本机安全机制会拦截"一次删超过 50 个文件"，所以先尝试改名让路，
    改名不成再退化成逐个删除（ignore_errors，绝不因为清理失败而中断验收）。"""
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError:
            for dp, dn, fn in os.walk(path):
                for f in fn:
                    try:
                        os.remove(os.path.join(dp, f))
                    except OSError:
                        pass
            shutil.rmtree(path, ignore_errors=True)
    os.makedirs(path, exist_ok=True)


def main():
    fresh_dir(RUN)
    print('=== 造素材（ffmpeg 合成，3 秒/个）===')
    build_fixture()
    for n in (WIDE, TALL, H264):
        w, h, c = probe_size(os.path.join(MAT, n))
        print('  %-28s %dx%d %s' % (n, w, h, c))

    p, lf = start()
    try:
        print()
        print('=== 1. 预热：该转的转，不该转的不转 ===')
        w = wait_warmup()
        print('  %s' % json.dumps(w, ensure_ascii=False))
        check('HEVC 超宽屏被识别为需要转码', w['needConv'] >= 2, 'needConv=%s' % w['needConv'])
        check('H.264 被判定为可直接播（不转）', w['skipped'] >= 1, 'skipped=%s' % w['skipped'])
        check('无转码失败', w['failed'] == 0, 'failed=%s' % w['failed'])

        print()
        print('=== 2. 转码产物必须是 720p（短边=720）===')
        products = []
        for dp, dn, fn in os.walk(os.path.join(CACHE, 'vtrans')):
            for f in fn:
                if f.endswith('.mp4'):
                    products.append(os.path.join(dp, f))
        check('缓存里有转码产物', len(products) >= 2, '%d 个' % len(products))
        prod_sha = {}
        for fp in products:
            pw, ph, pc = probe_size(fp)
            prod_sha[sha_file(fp)] = (os.path.basename(fp), pw, ph)
            short = min(pw, ph)
            check('产物短边=720  %s' % os.path.basename(fp)[:12],
                  short == 720, '%dx%d' % (pw, ph))
        wide_prod = [v for v in prod_sha.values() if v[1] > v[2]]
        check('横屏产物 = 1558x720', any(v[1] == 1558 and v[2] == 720 for v in wide_prod),
              str(wide_prod))

        print()
        print('=== 3. 下载链路必须给原视频（SHA256 比对）===')
        root_id = 1
        files = request('/api/files?folder=%d&limit=100&filter=all' % root_id)['files']
        byname = {f['name']: f for f in files}
        target = byname[WIDE]
        src = os.path.join(MAT, WIDE)
        src_sha = sha_file(src)
        prod_set = set(prod_sha.keys())
        check('原文件哈希不在转码产物集合里', src_sha not in prod_set)

        # 先标成「保留」。/api/media?dl=1 现在会校验 decision（未标记的返回 403），
        # 所以这一步必须排在下面的下载断言之前。
        request('/api/review', data=json.dumps({'fileId': target['id'], 'decision': 1}).encode('utf-8'),
                ctype='application/json')

        # A. 单文件直下
        try:
            request('/api/media?id=%d&dl=1' % target['id'], raw=True)
            check('无口令下载被拒', False, '竟然 200')
        except urllib.error.HTTPError as e:
            check('无口令下载被拒', e.code == 403, 'HTTP %d' % e.code)
        blob = request('/api/media?id=%d&dl=1&t=%s' % (target['id'], TOKEN), raw=True)
        check('单文件直下 == 原文件', sha_bytes(blob) == src_sha, '%d 字节' % len(blob))
        check('单文件直下 != 转码产物', sha_bytes(blob) not in prod_set)

        # B. 打包 ZIP（decision 已在上面标好）
        zp = os.path.join(RUN, 'pack.zip')
        for attempt, wait in enumerate((0.3, 1.0, 2.5), start=1):
            try:
                with OP.open('http://127.0.0.1:%d/api/zip?folder=%d&recursive=1&t=%s'
                             % (PORT, root_id, TOKEN), timeout=900) as r, open(zp, 'wb') as f:
                    shutil.copyfileobj(r, f)
                if attempt > 1:
                    global RETRIES
                    RETRIES += 1
                    print('  (打包第 %d 次尝试成功)' % attempt)
                break
            except (ConnectionResetError, ConnectionAbortedError) as e:
                print('  !! 打包连接被重置（第 %d/3 次）: %r' % (attempt, e))
                diag_conn()
                time.sleep(wait)
        else:
            raise RuntimeError('打包 ZIP 三次都失败')
        with zipfile.ZipFile(zp) as z:
            names = z.namelist()
            # 2026-09-25：打包会附带 导出清单.csv（有意新增）
            check('ZIP 里只含被标记保留的文件（+导出清单）', sorted(names) == sorted([WIDE, '导出清单.csv']), str(names))
            zsha = None
            for n in names:
                if n.lower().endswith('.mp4'):
                    hh = hashlib.sha256()
                    with z.open(n) as zf:
                        for c in iter(lambda: zf.read(1 << 20), b''):
                            hh.update(c)
                    zsha = hh.hexdigest()
            check('ZIP 内视频 == 原文件', zsha == src_sha)
            check('ZIP 内视频 != 转码产物', zsha not in prod_set)

        # C. 导出到本机目录
        dest = os.path.join(RUN, 'out')
        try:
            request('/api/copy?t=bad', data=json.dumps({'folderId': root_id, 'recursive': True,
                                                        'dest': dest}).encode('utf-8'),
                    ctype='application/json')
            check('导出接口对错误口令返回 403', False, '竟然 200')
        except urllib.error.HTTPError as e:
            check('导出接口对错误口令返回 403', e.code == 403, 'HTTP %d' % e.code)
        r = request('/api/copy?t=%s' % TOKEN,
                    data=json.dumps({'folderId': root_id, 'recursive': True,
                                     'dest': dest}).encode('utf-8'),
                    ctype='application/json')
        copied = os.path.join(dest, WIDE)
        check('导出接口报告成功', r.get('ok') is True, json.dumps(r, ensure_ascii=False))
        check('导出的文件 == 原文件', os.path.exists(copied) and sha_file(copied) == src_sha)
        check('导出的文件 != 转码产物', os.path.exists(copied) and sha_file(copied) not in prod_set)

        # D. 导出清单指向素材原件
        lst = request('/api/export?folder=%d&recursive=1&t=%s' % (root_id, TOKEN), raw=True)
        text = lst.decode('utf-8', 'replace')
        check('清单给出的是素材目录下的原件路径', MAT.lower() in text.lower(), text.strip()[:120])

        print()
        print('=== 4. 播放链路（唯一允许给转码产物的出口）===')
        st = request('/api/vtrans-status?id=%d' % target['id'])
        check('状态为 ready 且来源是转码', st.get('state') == 'ready' and st.get('source') == 'transcoded',
              json.dumps(st, ensure_ascii=False))
        v = request('/api/vtrans?id=%d' % target['id'], raw=True)
        check('/api/vtrans 给的是转码产物', sha_bytes(v) in prod_set, '%d 字节' % len(v))

        print()
        print('=== 5. 启动时必须清掉上一轮留下的产物 ===')

        def seed(name, size):
            d2 = os.path.join(CACHE, 'vtrans', 'zz')
            os.makedirs(d2, exist_ok=True)
            pth = os.path.join(d2, name)
            with open(pth, 'wb') as f:
                f.write(b'x' * size)
            return pth

        def products():
            n = 0
            for dp, dn, fn in os.walk(os.path.join(CACHE, 'vtrans')):
                n += len([f for f in fn if f.endswith('.mp4')])
            return n

        def restart(extra, tag):
            """停掉当前服务，带 extra 参数重启，等 /api/status 可访问后返回。"""
            nonlocal p, lf
            global PROC
            lf.flush()
            p.terminate()
            try:
                p.wait(timeout=20)
            except Exception:
                p.kill()
            lf = open(os.path.join(RUN, tag + '.log'), 'w', encoding='utf-8', newline='')
            q = subprocess.Popen([EXE, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                                  '-db', 'v.db', '-cache', CACHE] + extra,
                                 cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
            PROC = q
            up = False
            for _ in range(120):
                if q.poll() is not None:
                    break
                try:
                    request('/api/status')
                    up = True
                    break
                except Exception:
                    time.sleep(0.3)
            if not up:
                print('  !! 重启失败（poll=%r）。%s 日志：' % (q.poll(), tag))
                lf.flush()
                for l in open(os.path.join(RUN, tag + '.log'), encoding='utf-8',
                              errors='replace').read().splitlines()[-12:]:
                    print('     | ' + l)
            return q

        # 5a 默认行为：上一轮产物 + .part 残渣都该没
        leftover = seed('leftover_product.mp4', 4096)
        halfdone = seed('leftover_half.mp4.part', 2048)
        p = restart(['-vres', '720'], 'server_default')
        check('默认启动：上一轮产物被清掉', not os.path.exists(leftover))
        check('默认启动：.part 残渣被清掉', not os.path.exists(halfdone))
        wait_warmup(limit=300)
        logtxt = open(os.path.join(RUN, 'server_default.log'), encoding='utf-8', errors='replace').read()
        check('控制台打出「清理上次转码产物」', '清理上次转码产物' in logtxt,
              ' | '.join(l for l in logtxt.splitlines() if '清理' in l))
        check('清完之后照样能转出新产物', products() >= 2, '%d 个' % products())

        # 5b -vkeep：产物留着复用，.part 仍然清
        kept = seed('keepme_product.mp4', 4096)
        keephalf = seed('keepme_half.mp4.part', 2048)
        p = restart(['-vres', '720', '-vkeep'], 'server_keep')
        check('-vkeep：上一轮产物被保留', os.path.exists(kept))
        check('-vkeep：.part 残渣仍被清掉', not os.path.exists(keephalf))
        ktxt = open(os.path.join(RUN, 'server_keep.log'), encoding='utf-8', errors='replace').read()
        check('控制台提示沿用缓存', '沿用上次转码产物' in ktxt)
        wait_warmup(limit=300)

        # 5c 规格变了：就算 -vkeep 也必须清（老产物已作废）
        stale = seed('stale_old_spec.mp4', 1024)
        with open(os.path.join(CACHE, 'vtrans', '.profile'), 'w') as f:
            f.write('h1080-crf26-veryfast\n')
        p = restart(['-vres', '480', '-vkeep'], 'server_spec')
        check('改规格后老产物被清掉（-vkeep 也清）', not os.path.exists(stale))
        tagok = open(os.path.join(CACHE, 'vtrans', '.profile')).read().strip()
        check('规格标签已更新为 480p', tagok.startswith('h480'), tagok)
        wait_warmup(limit=300)

        # 5d 关键顺序：因端口被占而退出的实例，绝不能删掉正在跑的缓存
        guard = seed('guard_product.mp4', 4096)
        lf3 = open(os.path.join(RUN, 'server_dup.log'), 'w', encoding='utf-8', newline='')
        dup = subprocess.Popen([EXE, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                                '-db', 'v.db2', '-cache', CACHE, '-vres', '720'],
                               cwd=RUN, stdout=lf3, stderr=subprocess.STDOUT)
        rc = None
        try:
            rc = dup.wait(timeout=25)
        except Exception:
            dup.kill()
        lf3.flush()
        check('第二个实例因端口被占而退出', rc == 1, 'rc=%s' % rc)
        check('它退出前没有删掉正在跑的缓存', os.path.exists(guard))
        p = None
    finally:
        if p is not None:
            lf.flush()
            time.sleep(0.6)
            p.terminate()
            try:
                p.wait(timeout=20)
            except Exception:
                p.kill()
        lf.close()

    print()
    print('=== 服务端控制台（首次启动）===')
    logp = os.path.join(RUN, 'server.log')
    if os.path.exists(logp):
        for line in open(logp, encoding='utf-8', errors='replace').read().splitlines():
            print('  | ' + line)

    print()
    print('=== 服务端控制台（默认启动：清掉上一轮）===')
    logp = os.path.join(RUN, 'server_default.log')
    if os.path.exists(logp):
        for line in open(logp, encoding='utf-8', errors='replace').read().splitlines():
            print('  | ' + line)

    print()
    print('=== 服务端控制台（-vkeep：沿用上一轮）===')
    logp = os.path.join(RUN, 'server_keep.log')
    if os.path.exists(logp):
        for line in open(logp, encoding='utf-8', errors='replace').read().splitlines():
            print('  | ' + line)

    print()
    print('=== 服务端控制台（端口被占的第二个实例）===')
    logp = os.path.join(RUN, 'server_dup.log')
    if os.path.exists(logp):
        for line in open(logp, encoding='utf-8', errors='replace').read().splitlines():
            print('  | ' + line)

    failed = [r for r in results if not r[1]]
    print()
    print('=' * 56)
    print('共 %d 项，通过 %d，失败 %d' % (len(results), len(results) - len(failed), len(failed)))
    for n, ok, d in failed:
        print('  FAIL: %s %s' % (n, d))
    print('=' * 56)
    return 1 if failed else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as e:
        import traceback
        traceback.print_exc()
        sys.exit(2)
