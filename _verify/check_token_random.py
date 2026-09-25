# -*- coding: utf-8 -*-
"""验收：不传 -token 时，下载口令每次启动都必须重新随机生成。

背景：以前 start.bat 把口令写死成固定的一串，下载页地址因此可以收藏 ——
方便是方便，但拿到过一次地址的人以后永远能下载。现在改成每次随机，
这个脚本就是防它哪天被改回去的回归断言。

另外顺带断言：随机源读不到时不能静默降级成 "letmein"（那是把权限直接送人），
现在的实现是 log.Fatalf 直接退出。

用法：python _verify/check_token_random.py [exe路径]
"""
import os
import subprocess
import sys
import time

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.abspath(sys.argv[1]) if len(sys.argv) > 1 else os.path.join(D, 'medreview.exe')
RUN = os.path.join(D, '_verify', 'tokchk')

results = []


def check(name, ok, detail=''):
    results.append((name, bool(ok), detail))
    print('  [%s] %s%s' % ('PASS' if ok else 'FAIL', name, ('  -- ' + detail) if detail else ''))
    sys.stdout.flush()


def fresh_dir(path):
    """只改名让路，不真删（本机安全机制会拦整轮累计删除）。"""
    if os.path.exists(path):
        try:
            os.replace(path, '%s_trash_%s' % (path, time.strftime('%H%M%S')))
        except OSError:
            pass
    os.makedirs(path, exist_ok=True)


def token_of(port, tag):
    """起一个隔离实例，返回 (下载口令, 控制台日志)。"""
    cwd = os.path.join(RUN, tag)
    os.makedirs(cwd, exist_ok=True)
    logp = os.path.join(cwd, 'out.log')
    lf = open(logp, 'wb')
    p = subprocess.Popen(
        [EXE, '-addr', '127.0.0.1:%d' % port,
         '-db', os.path.join(cwd, 't.db'), '-cache', os.path.join(cwd, 'cache')],
        cwd=cwd, stdout=lf, stderr=subprocess.STDOUT,
        creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    try:
        # urls.txt 是端口占住之后才写的，能读到就说明这次真的起来了
        uf = os.path.join(cwd, 'urls.txt')
        txt = ''
        for _ in range(60):
            if os.path.exists(uf):
                try:
                    txt = open(uf, encoding='utf-8').read()
                except OSError:
                    txt = ''
                if txt.strip():
                    break
            time.sleep(0.5)
        return txt, p
    finally:
        pass


def main():
    print('exe =', EXE)
    if not os.path.exists(EXE):
        print('找不到 exe:', EXE)
        return 2
    fresh_dir(RUN)

    procs = []
    try:
        print()
        print('=== A. 两次启动的口令必须不同 ===')
        t1, p1 = token_of(8141, 'a')
        procs.append(p1)
        t1_line = [l for l in t1.splitlines() if 'download.html' in l]
        tok1 = t1_line[0].split('?t=')[1].strip() if t1_line and '?t=' in t1_line[0] else ''
        check('第一次启动写出了下载页地址', bool(t1_line), repr(t1_line[:1]))
        check('第一次的口令是 24 位 hex', len(tok1) == 24 and all(c in '0123456789abcdef' for c in tok1), tok1)

        p1.terminate()
        try:
            p1.wait(timeout=15)
        except Exception:
            p1.kill()
        time.sleep(1)

        t2, p2 = token_of(8142, 'b')
        procs.append(p2)
        t2_line = [l for l in t2.splitlines() if 'download.html' in l]
        tok2 = t2_line[0].split('?t=')[1].strip() if t2_line and '?t=' in t2_line[0] else ''
        check('第二次启动写出了下载页地址', bool(t2_line), repr(t2_line[:1]))
        check('两次口令不一样（每次启动都换）', tok1 != tok2 and len(tok2) == 24,
              '%s vs %s' % (tok1, tok2))

        print()
        print('=== B. 显式指定 -token 时按指定的来（想固定还是可以的）===')
        cwd = os.path.join(RUN, 'c')
        os.makedirs(cwd, exist_ok=True)
        lf = open(os.path.join(cwd, 'out.log'), 'wb')
        p3 = subprocess.Popen(
            [EXE, '-addr', '127.0.0.1:8143', '-token', 'myfixedpw',
             '-db', os.path.join(cwd, 't.db'), '-cache', os.path.join(cwd, 'cache')],
            cwd=cwd, stdout=lf, stderr=subprocess.STDOUT,
            creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        procs.append(p3)
        uf = os.path.join(cwd, 'urls.txt')
        txt = ''
        for _ in range(60):
            if os.path.exists(uf):
                try:
                    txt = open(uf, encoding='utf-8').read()
                except OSError:
                    txt = ''
                if txt.strip():
                    break
            time.sleep(0.5)
        line = [l for l in txt.splitlines() if 'download.html' in l]
        check('显式 -token 时地址里就是那个口令',
              bool(line) and line[0].endswith('?t=myfixedpw'), repr(line[:1]))

        print()
        print('=== C. 源码断言（防改回去）===')
        main_go = open(os.path.join(D, 'main.go'), encoding='utf-8').read()
        # 注释里会提到 letmein 这个历史坑，断言要看的是代码里不再 return 它
        check('随机源失败时不再静默返回 letmein', 'return "letmein"' not in main_go)
        # 现在是 fatalf（无黑窗版会把日志弹出来再退），不是裸的 log.Fatalf
        check('生成失败会 fatalf 退出', 'fatalf("无法生成随机下载口令' in main_go)
        check('口令是 12 字节熵（24 个 hex 字符）', 'make([]byte, 12)' in main_go)
        bat = os.path.join(D, 'start.bat')
        if os.path.exists(bat):
            bat_txt = open(bat, encoding='gbk', errors='replace').read()
            # 2026-09-25：start.bat 恢复使用且支持用户输入口令 ——
            # 禁止的是「写死具体口令」：这里只比对前缀，避免把历史口令原文写进公开仓库。
OLD_FIXED_TOKEN_PREFIX = 'b8cd6dd26'
            tok_lines = [l for l in bat_txt.splitlines() if '-token' in l]
            ok = all(('!TOKEN!' in l or '-token <你的口令>' in l) for l in tok_lines) and OLD_FIXED_TOKEN_PREFIX not in bat_txt
            check('start.bat 不写死口令（只用 !TOKEN! 变量）', ok,
                  str(tok_lines))
        else:
            check('start.bat 已移除（不再用 bat 启动）', True)
        rd = open(os.path.join(D, 'README.md'), encoding='utf-8').read()
        check('README 里不再出现那串固定口令', OLD_FIXED_TOKEN_PREFIX not in rd)
    finally:
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=10)
                except Exception:
                    p.kill()

    print()
    print('=' * 60)
    failed = [r for r in results if not r[1]]
    print('共 %d 项，通过 %d，失败 %d' % (len(results), len(results) - len(failed), len(failed)))
    print('=' * 60)
    for n, _, d in failed:
        print('  FAIL: %s  %s' % (n, d))
    return 1 if failed else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception:
        import traceback
        traceback.print_exc()
        sys.exit(2)
