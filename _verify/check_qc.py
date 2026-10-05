# -*- coding: utf-8 -*-
"""medreview 验收：质量自动检测（QC）端到端。

自给自足：自己用 ffmpeg 造七类素材，自己起停服务、自己清场。
**不碰 medreview.exe / :8080**，用独立的临时端口和临时 cwd 跑，跑完杀干净进程。

覆盖七个检测器：
  1. 空镜   纯色图 → bit2
  2. 重复   两份字节完全相同的图 → 至少一份 bit0（跨文件 pHash 聚合）
  3. 损坏   截断的假 .jpg → bit1
  4. 脏污   噪点底 + 平滑亮斑 → bit3 且 qc.boxes 归一化落在 [0,1]
  5. 模糊   噪点底 + 重度 boxblur → bit4
  6. 曝光   欠曝（大片死黑）/ 过曝（大片死白）→ bit5，且区分 under/over
  7. 噪点   渐变底 + 空间噪声 → bit6
外加 /api/qc/status 计数、SSE 广播、真实素材零误报。

用法：
    python _verify/check_qc.py                # 验 medreview_qcbuild.exe（没有就 medreview.exe）
    python _verify/check_qc.py medreview.exe  # 验指定的 exe（部署后复验用这个）

判定标准全在最后的 PASS/FAIL 表里，有 FAIL 就以非 0 退出码结束。
"""
import os
import sys
import json
import time
import shutil
import subprocess
import threading
import urllib.error
import urllib.request

try:
    from PIL import Image, ImageDraw  # 连拍/污损对账的端到端素材需要写 EXIF
    HAVE_PIL = True
except ImportError:
    HAVE_PIL = False

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.abspath(sys.argv[1]) if len(sys.argv) > 1 else os.path.join(D, 'medreview_qcbuild.exe')
if not os.path.exists(EXE):
    EXE = os.path.join(D, 'medreview.exe')
FF = os.path.join(D, 'tools', 'ffmpeg', 'ffmpeg.exe')
RUN = os.path.join(D, '_verify', 'qc')
MAT = os.path.join(RUN, 'mat')
CACHE = os.path.join(RUN, 'cache')
PORT = 8131
TOKEN = 'qct'
# 可选：把真实照片目录填到这里，可启用「真实素材零误报 / 连拍近似重复对账」端到端断言。
# 留空（默认）则只用 ffmpeg 确定性生成的兜底素材，验收照样完整通过。也可用环境变量覆盖：
#   MEDREVIEW_QC_REAL_SRC=/path/to/photos python _verify/check_qc.py
REAL_SRC = os.environ.get('MEDREVIEW_QC_REAL_SRC', r'')

F_DUP, F_CORRUPT, F_BLANK, F_DIRT = 1, 2, 4, 8
F_BLUR, F_EXPOSURE, F_NOISE = 16, 32, 64

OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
results = []
PROC = None
SSE_EVENTS = []
DUP_FROM_REAL = False   # 重复对用的是真实图（干净）还是生成的兜底图


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def request(path, raw=False, data=None, ctype=None, user='verifier'):
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
            time.sleep(wait)
    raise last


def fresh_dir(path):
    if os.path.exists(path):
        trash = '%s_trash_%s' % (path, time.strftime('%H%M%S'))
        try:
            os.replace(path, trash)
        except OSError as e:
            print('  !! 无法让开旧目录 %s: %r' % (path, e))
            print('     请手工删掉它再跑（脚本不真删，避免撞安全拦截）')
            raise SystemExit(2)
    os.makedirs(path, exist_ok=True)


def run_ff(args):
    p = subprocess.run([FF] + args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
    if p.returncode != 0:
        raise RuntimeError('ffmpeg 失败: %s\n%s' % (' '.join(args), p.stderr.decode('utf-8', 'replace')[:600]))


def build_fixture():
    """造素材。全部**确定性**生成（钉死 seed/颜色），否则阈值会周期性飘。"""
    global DUP_FROM_REAL
    os.makedirs(MAT, exist_ok=True)
    J = lambda n: os.path.join(MAT, n)

    # 1) 空镜：纯色灰
    run_ff(['-y', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=0x808080:s=640x480',
            '-frames:v', '1', J('blank.jpg')])

    # 2) 重复对：优先用一张真实图（实测 flags=0，最干净的底子）拷两份。
    #    真实图没有就退回落定的渐变，那种情况下"不被误判"的断言会跳过。
    real_jpgs = []
    if os.path.isdir(REAL_SRC):
        real_jpgs = sorted([f for f in os.listdir(REAL_SRC)
                            if f.lower().endswith(('.jpg', '.jpeg', '.png'))])
    if real_jpgs:
        DUP_FROM_REAL = True
        src = os.path.join(REAL_SRC, real_jpgs[0])
        shutil.copy(src, J('dup1.jpg'))
        shutil.copy(src, J('dup2.jpg'))
        real_rest = real_jpgs[1:]
    else:
        # ⚠️ gradients 默认 seed 随机，必须钉死；另外渐变本身很平滑，可能被判模糊 → 跳过误判断言
        run_ff(['-y', '-v', 'error', '-f', 'lavfi',
                '-i', 'gradients=s=640x480:c0=0x000000:c1=0xffffff:seed=42',
                '-frames:v', '1', J('_base.jpg')])
        shutil.copy(J('_base.jpg'), J('dup1.jpg'))
        shutil.copy(J('_base.jpg'), J('dup2.jpg'))
        os.remove(J('_base.jpg'))
        real_rest = []

    # 3) 损坏：随机字节的假 jpg（扫描按扩展名认成图片，解码必失败）
    with open(J('corrupt.jpg'), 'wb') as f:
        f.write(os.urandom(40))

    # 4) 脏污：随机静态噪点底（高方差，背景块不平滑）+ 中心平滑亮斑
    run_ff(['-y', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=black:s=640x480',
            '-vf', "geq=lum='if(lt(sqrt((X-320)*(X-320)+(Y-240)*(Y-240)),120),230,random(1)*255)':cb=128:cr=128",
            '-frames:v', '1', J('dirt.jpg')])

    # 5) 模糊：真实图 + 重度 boxblur（最贴近真实失焦场景）。
    #    ⚠️ 别拿"噪点底 + boxblur"当模糊素材：噪点一糊就变成接近纯色，会被判成**空镜**
    #    （而空镜会按设计抑制模糊标记），那样验的就不是模糊了。
    if real_jpgs:
        run_ff(['-y', '-v', 'error', '-i', os.path.join(REAL_SRC, real_jpgs[0]),
                '-vf', 'boxblur=14:2', '-frames:v', '1', J('blur.jpg')])
    else:
        # 兜底：渐变整体平滑（拉普拉斯≈0）但标准差够大，糊一下也只是糊，不会变纯色
        run_ff(['-y', '-v', 'error', '-f', 'lavfi',
                '-i', 'gradients=s=640x480:c0=0x000000:c1=0xffffff:seed=42',
                '-vf', 'boxblur=8:2', '-frames:v', '1', J('blur.jpg')])

    # 6) 曝光：左侧一条窄的暗/灰带，其余整片压到 255（过曝）或 0（欠曝）。
    #    留那条带是为了保持标准差够大（不然会同时命中空镜，说不清是哪个检测器在响）。
    #    ⚠️ 带子必须够窄：QC 是在 64×64 灰度上量直方图，带子一宽边界列会被平均掉，
    #    "近白占比"就掉到阈值以下（实测 X<100 时只剩 0.75 < 0.80，会漏检）。
    run_ff(['-y', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=black:s=640x480',
            '-vf', "geq=lum='if(lt(X,40),40,255)':cb=128:cr=128",
            '-frames:v', '1', J('over.jpg')])
    run_ff(['-y', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=black:s=640x480',
            '-vf', "geq=lum='if(lt(X,100),60,0)':cb=128:cr=128",
            '-frames:v', '1', J('under.jpg')])

    # 7) 噪点：钉死 seed 的渐变底 + 空间噪声（noise 不加 flag 就是逐像素噪声）
    run_ff(['-y', '-v', 'error', '-f', 'lavfi',
            '-i', 'gradients=s=640x480:c0=0x000000:c1=0xffffff:seed=42',
            '-vf', 'noise=alls=80', '-frames:v', '1', J('noisy.jpg')])

    # 8) 视频：纯色短视频 → 抽代表帧后应判空镜（验证视频也真的跑了检测）
    run_ff(['-y', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=0x707070:s=320x240:r=10',
            '-frames:v', '10', '-c:v', 'libx264', '-pix_fmt', 'yuv420p', J('flat.mp4')])

    # 真实素材拷进 real/ 子目录，单独校验零误报。
    # 只取 3 张：多了 QC 时间会被大图解码吃掉，跑一轮要好几分钟，没必要。
    real_rest = real_rest[:3]
    real_copied = []

    # 对账规则的端到端素材（同设备 EXIF，文件名 IMG_ 打头带拍摄时间）。
    # 需要 Pillow 写 EXIF；没有就跳过这组（单测已覆盖纯逻辑）。
    ISO_DIR = None
    if HAVE_PIL and os.path.isdir(REAL_SRC) and real_jpgs:
        ISO_DIR = os.path.join(MAT, 'real')
        os.makedirs(ISO_DIR, exist_ok=True)

        def save_photo(name, img, model):
            ex = Image.Exif()
            ex[272] = model  # Model：同设备分组的依据
            img.save(os.path.join(ISO_DIR, name), exif=ex, quality=92)

        # 连拍三连：同一秒 + 同一底图混入不同轻噪声。
        # ⚠️ 两个坑（都实测过）：底图别用渐变——它的 DCT 能量集中在 DC，
        # pHash 二值化后对微小噪声极敏感（hash 距离 14+，假阴性）；色块别移动位置——
        # 位置一变距离 26。正确做法 = 真实照片当底图 + 6% 高斯噪声（连拍的真实模型：
        # 同场景同构图，只有传感器噪声不同 → hash 距离 0~5）。
        base = Image.open(os.path.join(REAL_SRC, real_jpgs[0])).convert('RGB').resize((640, 480))
        for i, sec in enumerate(('01', '02', '03')):
            nz = Image.effect_noise((640, 480), 60).convert('RGB')
            im = Image.blend(base, nz, 0.06)
            save_photo('IMG_20260926_2200%s.jpg' % sec, im, 'BurstCam')

        # 单张独有脏斑：噪声底 + 平滑亮斑（会命中脏污检测），但同设备其余照片
        # 都没有同位置的脏点 → 对账后脏污位应被**清除**（这正是用户最想要的降噪）。
        # ⚠️ 必须用**独立设备名**（IsoCam）：这批素材和连拍素材混在一个 real/ 目录里，
        # 若与连拍图（真实照片内容）同设备，真实照片自己的脏点框可能恰好与测试斑
        # 重叠，把"单张独有"错误地"确认"成镜头污点 → 测试就不干净了。
        noise = Image.effect_noise((640, 480), 60).convert('RGB')
        d = ImageDraw.Draw(noise)
        d.ellipse([220, 140, 420, 340], fill=(230, 230, 230))
        save_photo('IMG_20260926_230001.jpg', noise, 'IsoCam')
        for i, sec in enumerate(('02', '03', '04')):
            save_photo('IMG_20260926_2300%s.jpg' % sec,
                       Image.linear_gradient('L').resize((640, 480)).convert('RGB'), 'IsoCam')

    if real_rest:
        rdir = os.path.join(MAT, 'real')
        os.makedirs(rdir, exist_ok=True)
        for nm in real_rest:
            shutil.copy(os.path.join(REAL_SRC, nm), os.path.join(rdir, nm))
            real_copied.append(nm)
    return real_copied, ISO_DIR


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
    for l in open(os.path.join(RUN, 'server.log'), encoding='utf-8', errors='replace').read().splitlines()[-12:]:
        print('     | ' + l)
    raise RuntimeError('服务未能启动')


def scan_prog():
    return request('/api/status').get('scan') or {}


def wait_scan(limit=60):
    t0 = time.time()
    while time.time() - t0 < limit:
        sc = scan_prog()
        if (not sc.get('running')) and (sc.get('files') or 0) > 0 and (sc.get('endedAt') or 0) > 0:
            return sc
        time.sleep(0.2)
    return scan_prog()


def wait_qc(expect, limit=300):
    """等所有素材都有了 qc 字段（QC 后台跑完）。

    ⚠️ 超时一定要喊出来：静默返回半截结果会让"还没算完"伪装成"flags=0"，
    看起来像检测器失灵，其实是没跑到 —— 加检测器后每张图多一次 1024 提取，明显变慢。
    """
    t0 = time.time()
    while time.time() - t0 < limit:
        d = request('/api/files?folder=1&limit=500&filter=all')
        files = {f['name']: f for f in d.get('files', [])}
        if len(files) >= expect and all('qc' in f for f in files.values()):
            return files
        time.sleep(0.3)
    d = request('/api/files?folder=1&limit=500&filter=all')
    files = {f['name']: f for f in d.get('files', [])}
    missing = [n for n, f in files.items() if 'qc' not in f]
    print('  !! QC 等待超时（%ds）：%d 个文件里还有 %d 个没算完 -> %s'
          % (limit, len(files), len(missing), missing[:8]))
    return files


def sse_reader():
    try:
        r = urllib.request.Request('http://127.0.0.1:%d/api/events' % PORT)
        resp = OP.open(r, timeout=120)
        for line in resp:
            line = line.decode('utf-8', 'replace').strip()
            if line.startswith('data:'):
                try:
                    SSE_EVENTS.append(json.loads(line[5:].strip()))
                except Exception:
                    pass
    except Exception:
        pass


def flags_of(f):
    return (f.get('qc') or {}).get('flags', 0)


def main():
    print('QC 验收：exe=%s' % EXE)
    fresh_dir(RUN)
    fresh_dir(MAT)
    real_copied, ISO_DIR = build_fixture()
    TOP = 10   # blank/dup1/dup2/corrupt/dirt/blur/over/under/noisy/flat.mp4
    print('  素材：%s' % sorted(os.listdir(MAT)))

    p, lf = start()
    threading.Thread(target=sse_reader, daemon=True).start()

    sc = wait_scan()
    print('  扫描完成：%s' % sc)
    files = wait_qc(expect=TOP)
    print('  QC 完成，本层文件数=%d' % len(files))
    for nm in sorted(files):
        q = files[nm].get('qc')
        print('     %-14s flags=%s' % (nm, q['flags'] if q else '<尚无qc>'))

    # ---- 空镜 ----
    blank = files.get('blank.jpg')
    if blank:
        check('空镜检测', flags_of(blank) & F_BLANK != 0, 'flags=%d' % flags_of(blank))
        # 纯色帧只该有空镜一个标记（模糊被刻意抑制，见 qc.go：空镜不再叠模糊）
        check('空镜帧不叠其它标记', flags_of(blank) == F_BLANK, 'flags=%d' % flags_of(blank))
    else:
        check('空镜检测', False, '素材缺失')

    # ---- 损坏 ----
    corrupt = files.get('corrupt.jpg')
    check('损坏检测', corrupt and flags_of(corrupt) & F_CORRUPT != 0,
          'flags=%s' % (flags_of(corrupt) if corrupt else '缺失'))

    # ---- 重复 ----
    d1, d2 = files.get('dup1.jpg'), files.get('dup2.jpg')
    which = [nm for nm, f in (('dup1', d1), ('dup2', d2)) if f and flags_of(f) & F_DUP]
    check('重复检测(跨文件 pHash)', bool(which), '命中=%s（两份相同图互标重复属正常）' % (which or '无'))
    if DUP_FROM_REAL:
        for nm, f in (('dup1', d1), ('dup2', d2)):
            if f:
                extra = flags_of(f) & ~F_DUP
                check('重复对不被误判(%s)' % nm, extra == 0, '多余位=%d flags=%d' % (extra, flags_of(f)))

    # ---- 脏污 ----
    dirt = files.get('dirt.jpg')
    dirt_ok = bool(dirt) and flags_of(dirt) & F_DIRT != 0
    boxes = (dirt.get('qc') or {}).get('boxes') if dirt else None
    box_ok = bool(boxes) and all(0 <= b.get('x', -1) <= 1 and 0 <= b.get('y', -1) <= 1 and
                                 0 < b.get('w', 0) <= 1 and 0 < b.get('h', 0) <= 1 for b in (boxes or []))
    check('镜头脏污检测', dirt_ok, 'flags=%s' % (flags_of(dirt) if dirt else '缺失'))
    check('脏污框坐标归一化且非空', box_ok, 'boxes=%s' % (boxes if boxes else '无'))

    # ---- 模糊 ----
    blur = files.get('blur.jpg')
    check('模糊检测', blur and flags_of(blur) & F_BLUR != 0, 'flags=%s' % (flags_of(blur) if blur else '缺失'))

    # ---- 曝光 ----
    over = files.get('over.jpg')
    under = files.get('under.jpg')
    o_ok = bool(over) and flags_of(over) & F_EXPOSURE != 0
    u_ok = bool(under) and flags_of(under) & F_EXPOSURE != 0
    check('过曝检测', o_ok, 'flags=%s' % (flags_of(over) if over else '缺失'))
    check('欠曝检测', u_ok, 'flags=%s' % (flags_of(under) if under else '缺失'))
    if over:
        check('过曝方向正确(expOver)', (over.get('qc') or {}).get('expOver') is True,
              'expOver=%s' % (over.get('qc') or {}).get('expOver'))
    if under:
        check('欠曝方向正确(expUnder)', (under.get('qc') or {}).get('expUnder') is True,
              'expUnder=%s' % (under.get('qc') or {}).get('expUnder'))

    # ---- 噪点 ----
    noisy = files.get('noisy.jpg')
    check('噪点检测', noisy and flags_of(noisy) & F_NOISE != 0,
          'flags=%s' % (flags_of(noisy) if noisy else '缺失'))

    # ---- 视频抽帧 QC ----
    vid = files.get('flat.mp4')
    check('视频也跑 QC（抽代表帧）', vid is not None and 'qc' in vid,
          'flags=%s' % (flags_of(vid) if vid else '无'))
    if vid and 'qc' in vid:
        check('纯色视频判为空镜', flags_of(vid) & F_BLANK != 0, 'flags=%d' % flags_of(vid))

    # ---- 质量筛选（纯视图过滤，不影响打包口径）----
    dups = {f['name']: f for f in
            request('/api/files?folder=1&limit=500&filter=qcdup').get('files', [])}
    check('筛选 qcdup 只出重复件', bool(dups) and all(flags_of(f) & F_DUP for f in dups.values()),
          '命中=%s' % sorted(dups))
    issues = {f['name']: f for f in
              request('/api/files?folder=1&limit=500&filter=qcissue').get('files', [])}
    # "问题件"不含纯重复（重复只是冗余，片子没毛病）
    ISSUE = F_CORRUPT | F_BLANK | F_DIRT | F_BLUR | F_EXPOSURE | F_NOISE
    check('筛选 qcissue 只出问题件', bool(issues) and all(flags_of(f) & ISSUE for f in issues.values()),
          '命中=%s' % sorted(issues))
    check('qcissue 不含纯重复件', all(not (flags_of(f) & F_DUP) or (flags_of(f) & ISSUE)
                                      for f in issues.values()),
          '命中=%s' % sorted(issues))

    # ---- /api/qc/status ----
    st = request('/api/qc/status?folder=1')
    total, ndup = st.get('total') or 0, st.get('dup') or 0
    ncorrupt, nblank, ndirt = st.get('corrupt') or 0, st.get('blank') or 0, st.get('dirt') or 0
    nblur, nexp, nnoise = st.get('blur') or 0, st.get('exposure') or 0, st.get('noise') or 0
    check('qc/status 总数正确', total == len(files), 'total=%s files=%d' % (total, len(files)))
    # blank 用 >= 1：纯色视频 flat.mp4 也会命中空镜，数量随素材而变
    check('qc/status 计数一致',
          ndup >= 1 and ncorrupt == 1 and nblank >= 1 and ndirt == 1 and
          nblur >= 1 and nexp >= 2 and nnoise >= 1,
          'dup=%s corrupt=%s blank=%s dirt=%s blur=%s exp=%s noise=%s'
          % (ndup, ncorrupt, nblank, ndirt, nblur, nexp, nnoise))

    # ---- SSE ----
    qc_evts = [e for e in SSE_EVENTS if e.get('type') == 'qc']
    check('SSE 广播 qc 事件', len(qc_evts) > 0, '收到 %d 条' % len(qc_evts))
    if qc_evts:
        d0 = qc_evts[0].get('data', {})
        check('qc 事件结构', 'fileId' in d0 and 'qc' in d0,
              'sample=%s' % json.dumps(qc_evts[0], ensure_ascii=False)[:140])

    # ---- 真实素材零误报（在 real/ 子目录，先定位它的 folder id）----
    real_folder = None
    if real_copied:
        try:
            for node in request('/api/folders?parent=1').get('folders', []):
                if node.get('name') == 'real':
                    real_folder = node.get('id')
        except Exception:
            pass
        if real_folder is not None:
            rfiles = {f['name']: f for f in
                      request('/api/files?folder=%d&limit=200&filter=all' % real_folder).get('files', [])}
            bad = [(nm, flags_of(rfiles[nm])) for nm in real_copied
                   if nm in rfiles and flags_of(rfiles[nm]) != 0]
            check('真实素材零误报', not bad, '误报=%s' % bad)
        else:
            print('  (跳过真实素材零误报断言：未定位到 real 子目录)')

    # ---- 跨照片对账（同设备 EXIF 素材）----
    if ISO_DIR and real_folder is not None:
        rfiles = {f['name']: f for f in
                  request('/api/files?folder=%d&limit=200&filter=all' % real_folder).get('files', [])}
        # 对账在 WarmAll 全部算完后跑，轮询等它把「单张独有的脏斑」标记清掉。
        # ⚠️ 每圈都要**重新拉数据**：不重拉的话读到的永远是同一个旧对象，
        # flags 永远不变，只能空转到超时（对账明明已经清掉了，白报一个 FAIL）。
        t0 = time.time()
        iso_flags = -1
        while time.time() - t0 < 25:
            f = rfiles.get('IMG_20260926_230001.jpg')
            if f is None or 'qc' not in f:
                iso_flags = -1
            else:
                iso_flags = flags_of(f)
                if iso_flags & F_DIRT == 0:
                    break
            time.sleep(0.5)
            rfiles = {x['name']: x for x in
                      request('/api/files?folder=%d&limit=200&filter=all' % real_folder).get('files', [])}
        check('单张独有的脏斑被对账清除',
              iso_flags >= 0 and iso_flags & F_DIRT == 0, 'flags=%s' % iso_flags)

        burst_dups = sorted(nm for nm, f in rfiles.items()
                            if nm.startswith('IMG_20260926_2200') and flags_of(f) & F_DUP)
        check('连拍近似重复被标出（≥2）', len(burst_dups) >= 2, '命中=%s' % burst_dups)

    try:
        p.terminate()
        p.wait(timeout=10)
    except Exception:
        try:
            p.kill()
        except Exception:
            pass
    print('  服务已停止，端口释放。')

    fails = [r for r in results if not r[1]]
    print('\n==== QC 验收结果：%d 项，%d 失败 ====' % (len(results), len(fails)))
    for nm, ok, det in results:
        if not ok:
            print('  FAIL  %s  %s' % (nm, det))
    raise SystemExit(1 if fails else 0)


if __name__ == '__main__':
    main()
