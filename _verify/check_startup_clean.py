# -*- coding: utf-8 -*-
"""专测「启动时清掉上一轮转码产物」这一条行为，四条分支：
  1. 默认：上一轮产物 + .part 残渣都被清
  2. 清完还能正常转出新产物
  3. -vkeep：产物保留，.part 仍清
  4. 改规格（-vkeep 也照清）+ 第二个实例因端口被占退出时不得删缓存

比 check_download_and_720p.py 轻量、只跑这一件事，稳定好重复。
用法：python _verify/check_startup_clean.py [exe路径]
"""
import os, sys, json, time, shutil, subprocess, urllib.request, urllib.error

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = sys.argv[1] if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
FF = os.path.join(D, 'tools', 'ffmpeg', 'ffmpeg.exe')
RUN = os.path.join(D, '_verify', 'clean')
MAT = os.path.join(RUN, 'mat')
CACHE = os.path.join(RUN, 'cache')
VT = os.path.join(CACHE, 'vtrans')
PORT = 8103
TOKEN = 'tk'
OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
results = []
PROC = None


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def fresh_dir(path):
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError:
            shutil.rmtree(path, ignore_errors=True)
    os.makedirs(path, exist_ok=True)


def get(path, raw=False):
    r = urllib.request.Request('http://127.0.0.1:%d%s' % (PORT, path))
    r.add_header('X-User', 'verifier')
    b = OP.open(r, timeout=60).read()
    return b if raw else json.loads(b.decode('utf-8'))


def start(exe, extra, tag):
    """起一个实例，等到 /api/status 能通（= PrepareCache 已经跑完）。"""
    global PROC
    lf = open(os.path.join(RUN, tag + '.log'), 'w', encoding='utf-8', newline='')
    q = subprocess.Popen([exe, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                          '-db', 'v.db', '-cache', CACHE] + extra,
                         cwd=RUN, stdout=lf, stderr=subprocess.STDOUT)
    PROC = q
    up = False
    for _ in range(120):
        if q.poll() is not None:
            break
        try:
            get('/api/status')
            up = True
            break
        except Exception:
            time.sleep(0.3)
    if not up:
        print('  !! %s 启动失败 poll=%r，日志：' % (tag, q.poll()))
        lf.flush()
        for l in open(os.path.join(RUN, tag + '.log'), encoding='utf-8',
                      errors='replace').read().splitlines()[-10:]:
            print('     | ' + l)
    return q, lf


def stop(q, lf):
    if q is None:
        return
    lf.flush()
    time.sleep(0.4)
    q.terminate()
    try:
        q.wait(timeout=20)
    except Exception:
        q.kill()
    lf.close()


def warmup_started(w):
    """预热是否已经真的开始过。
    全零 = 还没启动（服务是「先监听、再异步扫描」，扫描完才触发预热），
    这不代表失败，只代表我们比扫描快了一步。"""
    return bool(w.get('startedAt') or w.get('finishedAt') or w.get('total')
                or w.get('probed') or w.get('needConv') or w.get('skipped')
                or w.get('cached') or w.get('failed') or w.get('done'))


def wait_warm(limit=180):
    """等预热跑完，两阶段。

    少了阶段一会误判：/api/status 一能连上就查预热，此时扫描可能还没跑完，
    读到的是从未启动过的全零状态（active 天然为 false）——
    偶发「第一轮转出了产物 0 个」就是这个竞态，不是产品 bug。
    """
    # 阶段一：等预热被触发（最多 60 秒）
    t0 = time.time()
    while time.time() - t0 < 60:
        if PROC.poll() is not None:
            return {}
        try:
            w = get('/api/vtrans-warmup')
        except Exception:
            time.sleep(0.3)
            continue
        if warmup_started(w):
            break
        time.sleep(0.2)
    # 阶段二：等它跑完
    t0 = time.time()
    while time.time() - t0 < limit:
        if PROC.poll() is not None:
            return {}
        try:
            w = get('/api/vtrans-warmup')
        except Exception:
            time.sleep(1)
            continue
        if not w.get('active'):
            return w
        time.sleep(1)
    return {}


def seed(name, size):
    d = os.path.join(VT, 'zz')
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, name)
    with open(p, 'wb') as f:
        f.write(b'x' * size)
    return p


def products():
    n = 0
    for dp, dn, fn in os.walk(VT):
        n += len([f for f in fn if f.endswith('.mp4')])
    return n


def logtext(tag):
    p = os.path.join(RUN, tag + '.log')
    return open(p, encoding='utf-8', errors='replace').read() if os.path.exists(p) else ''


def main():
    print('exe =', EXE)
    if not os.path.exists(EXE):
        print('找不到 exe'); return 2
    fresh_dir(RUN)
    os.makedirs(MAT, exist_ok=True)
    vid = os.path.join(MAT, 'a.mp4')
    # 造一个必须转码的 HEVC（短，转得快）
    subprocess.run([FF, '-hide_banner', '-y', '-f', 'lavfi', '-i', 'testsrc2=size=1280x1280:rate=15',
                    '-t', '1', '-c:v', 'libx265', '-crf', '26', '-preset', 'ultrafast',
                    '-pix_fmt', 'yuv420p', '-tag:v', 'hvc1', vid], capture_output=True)

    print()
    print('=== 0. 先跑一轮，拿到基准产物 ===')
    q, lf = start(EXE, ['-vres', '720'], 'p0')
    w = wait_warm()
    print('  预热:', json.dumps(w, ensure_ascii=False)[:120])
    check('第一轮转出了产物', products() >= 1, '%d 个' % products())
    stop(q, lf)

    print()
    print('=== 1. 默认再启动一次：上一轮产物必须没 ===')
    before = products()
    # 用一个 warmup 永远不会重建的文件当"上一轮产物"的样本，避免和"清完立刻重转"抢时间
    decoy = seed('decoy_prev_run.mp4', 4096)
    q, lf = start(EXE, ['-vres', '720'], 'p1')
    check('上一轮产物已被清掉', not os.path.exists(decoy))
    print('  (信息) 启动前产物 %d 个，启动后瞬间 %d 个（随后会重转）' % (before, products()))
    check('控制台打出「清理上次转码产物」', '清理上次转码产物' in logtext('p1'),
          ' | '.join(l for l in logtext('p1').splitlines() if '清理' in l))
    w = wait_warm()
    check('清完之后照样能转出新产物', products() >= 1, '%d 个' % products())

    print()
    print('=== 2. .part 残渣必须清（任何情况）===')
    stop(q, lf)
    half = seed('leftover.mp4.part', 1024)
    q, lf = start(EXE, ['-vres', '720'], 'p2')
    check('.part 残渣被清掉', not os.path.exists(half))
    wait_warm()

    print()
    print('=== 3. -vkeep：产物保留，.part 仍清 ===')
    stop(q, lf)
    kept = seed('keepme.mp4', 4096)
    half2 = seed('half2.mp4.part', 2048)
    q, lf = start(EXE, ['-vres', '720', '-vkeep'], 'p3')
    check('-vkeep 时产物被保留', os.path.exists(kept))
    check('-vkeep 时 .part 仍被清掉', not os.path.exists(half2))
    check('控制台提示沿用缓存', '沿用上次转码产物' in logtext('p3'))
    wait_warm()

    print()
    print('=== 4. 改了规格：-vkeep 也必须清 ===')
    stop(q, lf)
    stale = seed('stale_old_spec.mp4', 1024)
    with open(os.path.join(VT, '.profile'), 'w') as f:
        f.write('h1080-crf26-veryfast\n')
    q, lf = start(EXE, ['-vres', '480', '-vkeep'], 'p4')
    check('改规格后老产物被清掉', not os.path.exists(stale))
    tag = open(os.path.join(VT, '.profile')).read().strip()
    check('规格标签已更新', tag.startswith('h480'), tag)
    wait_warm()

    print()
    print('=== 5. 端口被占的第二个实例不得删缓存 ===')
    guard = seed('guard.mp4', 4096)
    lf2 = open(os.path.join(RUN, 'p5dup.log'), 'w', encoding='utf-8', newline='')
    dup = subprocess.Popen([EXE, '-root', MAT, '-addr', ':%d' % PORT, '-token', TOKEN,
                            '-db', 'v2.db', '-cache', CACHE, '-vres', '720'],
                           cwd=RUN, stdout=lf2, stderr=subprocess.STDOUT)
    rc = None
    try:
        rc = dup.wait(timeout=25)
    except Exception:
        dup.kill()
    lf2.flush()
    check('第二个实例因端口被占而退出', rc == 1, 'rc=%s' % rc)
    check('它退出前没有删掉正在跑的缓存', os.path.exists(guard))
    stop(q, lf)

    failed = [r for r in results if not r[1]]
    print()
    print('=' * 56)
    print('共 %d 项，通过 %d，失败 %d' % (len(results), len(results) - len(failed), len(failed)))
    for n, ok, d in failed:
        print('  FAIL: %s  %s' % (n, d))
    print('=' * 56)
    return 1 if failed else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception:
        import traceback
        traceback.print_exc()
        sys.exit(2)
