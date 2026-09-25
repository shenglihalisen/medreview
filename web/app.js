(function () {
  "use strict";

  // 手机版 / 电脑版由 <head> 里的内联脚本按浏览器特征判定（与窗口宽度无关），这里只读结果。
  var IS_PHONE = document.documentElement.classList.contains("is-phone");

  // 离线演示页（web_demo/demo.html）会预设 window.__MR_DEMO__；正式页面永远是 null。
  var DEMO = window.__MR_DEMO__ || null;

  var S = {
    mode: "review",
    token: "",
    user: "",
    userEntered: false,
    folderId: 1,
    folderName: "",
    folderRel: "",
    items: [],
    byId: {},
    cursorName: "",
    cursorId: 0,
    hasMore: true,
    loading: false,
    filter: "all",
    cols: 4,
    selfTotal: 0,
    pending: 0,
    hoverId: 0,
    // 幂等标记：buildStaticUI 只允许生效一次
    uiBuilt: false,
    // 「含父目录」：把上一层目录直属的照片一起显示（默认开）
    withParent: true,
    // 画质：preview = 低清压缩图（快）；orig = 原图（慢但看细节）
    // 网格和灯箱默认都跟着它走；灯箱里的「看原图」是单张临时覆盖。
    quality: "preview",
    lbIndex: -1,
    // 灯箱当前这张是否在看原图（预览版默认；翻页/关灯箱自动回到预览版）
    lbOrig: false,
    // 「完成审阅」用：非 null 时灯箱只看这个数组，而不是当前目录的 S.items
    lbOverride: null,
    lbTitle: "",
    page: 60,
    claimedBy: "",
    canDownload: false,
    rowH: 220,
    colW: 200,
    cellH: 210,
    gap: 10,
    // 当前屏幕上真正可见的那一段下标区间（左闭右开），由 render() 算出来。
    // 批量按钮只作用于这个区间 —— S.items 是"已载入"的（滚动会不断追加，
    // 每页 60），拿它当"本页"会让用户没看见的卡片也被勾上。
    visA: 0,
    visB: 0,
    // 卡片底部标记按钮条的高度。必须与 style.css 的 --acks-h 保持一致
    // （卡片几何是这里用 JS 算的，CSS 改了这里必须跟着改）。
    acksH: IS_PHONE ? 44 : 30,
    pool: new Map(),
    treeChildren: new Map(),
    expanded: new Set([1]),
    refreshTimer: 0,
    es: null,
    hiddenAt: 0,
    nameGateDone: null,
  };

  var el = {};
  function $(id) { return document.getElementById(id); }

  // 关键：本页可能被 WorkBuddy 的「应用内预览面板」渲染（那是另一个端口，
  // 例如 127.0.0.1:31976），此时相对路径的 /api/* 会打到预览服务而不是本服务，
  // 必然"加载失败"。只有这两种情况才需要指回真实端口：
  //   ① 页面是从本地文件打开的（file://，例如离线演示页）
  //   ② 页面由预览面板以 /static-html/... 提供
  // 其余一律用相对路径 —— 手机用内网 IP 访问、服务换端口，都能自动正确。
  var API_BASE = (DEMO && DEMO.apiBase !== undefined) ? DEMO.apiBase
    : ((location.protocol === "file:" || /^\/static-html\//.test(location.pathname))
      ? "http://127.0.0.1:8080"
      : "");

  function U(p) {
    // 离线演示页把 /api/media?id=N 映射到同目录 demo_img/ 下的真实图片
    if (DEMO && DEMO.mediaMap) {
      var m = /\/api\/media\?id=(\d+)/.exec(p);
      if (m && DEMO.mediaMap[m[1]]) return "demo_img/" + DEMO.mediaMap[m[1]];
    }
    return API_BASE + p;
  }

  // 跨域时（页面在 WorkBuddy 预览面板里打开），<img src> 会撞上预览服务的
  // CSP `img-src 'self' data: blob: https:`（不含 127.0.0.1），图片被拦；
  // 但 CSP 的 `connect-src` 放行了 http://127.0.0.1:*，所以用 fetch 取回 blob、
  // 再转成 blob: URL 赋给 <img>，预览面板里也能正常显示缩略图。
  function setImg(img, path, onFail) {
    if (!API_BASE) { img.src = path; return; }
    fetch(path, { headers: { "X-User": encodeURIComponent(S.user) } })
      .then(function (r) { if (!r.ok) throw new Error(String(r.status)); return r.blob(); })
      .then(function (b) { img.src = URL.createObjectURL(b); })
      .catch(function () { if (onFail) onFail(); });
  }

  // 视频：先问服务端这个文件浏览器能不能直接播。
  // 手机录屏常见 HEVC / H.265，Chrome/Edge 解不了，必须由服务端先转成 H.264；
  // 转码期间服务端会回报百分比，这里透传给界面显示进度。
  function videoSrc(id, onReady, onState) {
    return fetch(U("/api/vtrans-status?id=" + id), { headers: { "X-User": encodeURIComponent(S.user) } })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d && d.state === "ready") { onReady(U("/api/vtrans?id=" + id)); return true; }
        onState(d || { state: "error", msg: "未知状态" });
        return false;
      })
      .catch(function () {
        // 取不到状态（服务端偏旧/网络异常）：退回原文件，至少不比以前差
        onReady(U("/api/media?id=" + id));
        return true;
      });
  }

  // ---------- 基础请求 ----------
  function api(path, opts) {
    opts = opts || {};
    opts.headers = opts.headers || {};
    // HTTP 头只允许 ISO-8859-1；用户名可能是中文，必须编码，否则 fetch 直接抛异常
    opts.headers["X-User"] = encodeURIComponent(S.user);
    if (S.token) path += (path.indexOf("?") >= 0 ? "&" : "?") + "t=" + encodeURIComponent(S.token);
    return fetch(U(path), opts).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.statusText); });
      return r.json();
    });
  }

  function toast(msg) {
    var t = el.toast;
    t.textContent = msg;
    t.classList.add("on");
    clearTimeout(t._timer);
    t._timer = setTimeout(function () { t.classList.remove("on"); }, 2200);
  }

  // 请求失败时给出可操作的提示。最常见的"加载失败"是页面与服务的地址不一致：
  // 预览面板 / 本地文件打开时，请求打到别处；手机上则该确认用的是内网 IP。
  function netHint(e) {
    var m = (e && e.message) || "";
    if (/non ISO-8859-1/i.test(m)) {
      return "（请求头含非 ASCII 字符，通常是用户名有中文——新版本已修复）";
    }
    if (/Failed to fetch|NetworkError|Load failed/i.test(m)) {
      return "（连不上服务：确认服务已启动；若在应用内预览面板打开，请改用系统浏览器访问" +
        " http://127.0.0.1:8080/review.html；手机请用启动窗口里打印的局域网地址）";
    }
    return "";
  }

  function fmtSize(n) {
    if (!n) return "0";
    var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + u[i];
  }

  // ---------- 初始化 ----------
  function init(mode) {
    S.mode = mode;
    el = {
      tree: $("tree"), grid: $("grid"), wrap: $("grid-wrap"), crumb: $("crumb"),
      toolbar: $("toolbar"), scanbar: $("scanbar"), lightbox: $("lightbox"),
      lbBody: $("lb-body"), lbName: $("lb-name"), lbPrev: $("lb-prev"), lbNext: $("lb-next"),
      lbCount: $("lb-count"), lbZin: $("lb-zin"), lbZout: $("lb-zout"), lbOrig: $("lb-orig"),
      modal: $("modal"), modalBody: $("modal-body"), modalTitle: $("modal-title"),
      nameGate: $("name-gate"), nameInput: $("name-input"), nameHist: $("name-hist"), nameOk: $("name-ok"),
      toast: $("toast"), stat: $("stat"), claimBtn: $("claim-btn"), finishBtn: $("btn-finish"),
      empty: $("empty"), dlBar: $("dl-bar"),
    };

    var q = new URLSearchParams(location.search);
    var tk = q.get("t");
    if (tk) { S.token = tk; try { localStorage.setItem("mr_token", tk); } catch (e) {} }
    else { try { S.token = localStorage.getItem("mr_token") || ""; } catch (e) {} }

    // 列数偏好只在电脑版上读写（手机固定 1 列，不去污染桌面偏好）。
    // localStorage 一律包 try/catch：在存储被禁的上下文里抛异常会让 init 中断，
    // 后面 buildStaticUI()/bindEvents() 全都不会执行 —— 表现就是"所有按钮都没反应"。
    if (!IS_PHONE) {
      var savedCols = 4;
      try { savedCols = parseInt(localStorage.getItem("mr_cols") || "4", 10) || 4; } catch (e) {}
      S.cols = savedCols;
    }
    // 「含父目录」开关：默认开，记住选择（同样包 try）
    try { S.withParent = localStorage.getItem("mr_parent") !== "0"; } catch (e) {}
    // 画质偏好（压缩图 / 原图）
    try { S.quality = localStorage.getItem("mr_quality") === "orig" ? "orig" : "preview"; } catch (e) {}

    // 每次打开/刷新都必须重新登记批注名，且不能跳过：
    // 不读缓存、不写名字缓存（cookie 只作服务端兜底），刷新就一定重填。
    // 同一页面生命周期内没有改名入口 —— 要改只能刷新。
    openNameGate(function (name) {
      S.user = (name || "").trim();
      S.userEntered = true;
      try { localStorage.removeItem("mr_user"); } catch (e) {}
      try { document.cookie = "mr_user=" + encodeURIComponent(S.user) + ";path=/;max-age=864000"; } catch (e) {}

      buildStaticUI();
      bindEvents();

      loadStatus().then(function () {
        S.folderId = 1;
        return loadTree(0);
      }).then(function () {
        renderTree();
        // 离线演示页可以指定初始目录（DEMO.startFolder），正式页面永远是根目录
        return selectFolder((DEMO && DEMO.startFolder) || 1);
      }).catch(function (e) { toast("加载失败: " + e.message + netHint(e)); });

      connectSSE();
    });
  }

  // ---------- 批注名门禁 ----------
  // 网页内弹窗（不再用原生 prompt：微信内置浏览器 / iOS 独立 WebApp 会拦掉它，
  // 而沙箱 iframe 里 prompt 会被忽略，直接静默降级成访客）。
  // 强制填写：没有"跳过"，输入为空时「开始审阅」是灰的。
  function openNameGate(onDone) {
    // 离线演示页免门禁
    if (DEMO && DEMO.userEntered) { onDone(DEMO.user || "演示用户"); return; }

    S.nameGateDone = null;
    el.nameInput.value = "";
    el.nameOk.disabled = true;
    el.nameGate.classList.add("on");

    var submit = function () {
      var v = (el.nameInput.value || "").trim();
      if (!v) return;                       // 强制模式不接受空
      el.nameGate.classList.remove("on");
      var cb = S.nameGateDone;
      S.nameGateDone = null;
      onDone(v);
      if (cb) cb(v);
    };
    S.nameGateDone = submit;

    el.nameInput.oninput = function () { el.nameOk.disabled = !(el.nameInput.value || "").trim(); };
    el.nameOk.onclick = submit;

    // 已经登录过的用户名（服务端进程内存，关掉程序就没了）
    api("/api/users").then(function (d) {
      var list = (d && d.users) || [];
      // 保留第一个占位 option
      while (el.nameHist.options.length > 1) el.nameHist.remove(1);
      list.forEach(function (u) {
        var o = document.createElement("option");
        o.value = u; o.textContent = u;
        el.nameHist.appendChild(o);
      });
      el.nameHist.disabled = list.length === 0;
    }).catch(function () { el.nameHist.disabled = true; });
    el.nameHist.value = "";
    el.nameHist.onchange = function () {
      if (!el.nameHist.value) return;
      el.nameInput.value = el.nameHist.value;
      el.nameOk.disabled = false;
    };

    // 手机上程序化 focus 常失败且会引起页面位移，只在鼠标环境下自动聚焦
    if (window.matchMedia("(pointer: fine)").matches) {
      setTimeout(function () { try { el.nameInput.focus(); } catch (e) {} }, 30);
    }
  }

  // 标记前的最后一道闸：名字没登记就不许落标记（正常流程下门禁已强制填过，
  // 这里是防止 ?ui= / 演示页 / 异常路径漏过去的安全网）。
  function guardName(then) {
    if (S.userEntered && S.user) { then(); return; }
    openNameGate(function (name) {
      var v = (name || "").trim();
      if (!v) return;
      S.user = v;
      S.userEntered = true;
      try { document.cookie = "mr_user=" + encodeURIComponent(S.user) + ";path=/;max-age=864000"; } catch (e) {}
      var who = $("who");
      if (who) who.textContent = "当前用户: " + S.user;
      updateStat();
      then();
    });
  }

  function buildStaticUI() {
    // 幂等：不管被调几次，工具栏里都只该有一份「列数/筛选」。
    // 槽位清空 + 去重标记双保险，避免任何重复调用 / 重复注入导致控件并列出现两个。
    if (S.uiBuilt) return;
    S.uiBuilt = true;
    var sc = $("slot-cols"), sf = $("slot-filter");
    if (sc) sc.innerHTML = "";
    if (sf) sf.innerHTML = "";

    // 列数
    var colsSeg = document.createElement("div");
    colsSeg.className = "seg";
    colsSeg.id = "cols-seg";
    [1, 2, 3, 4, 6, 9].forEach(function (c) {
      var b = document.createElement("button");
      b.textContent = c + "列";
      b.dataset.cols = c;
      if (c === S.cols) b.classList.add("on");
      b.onclick = function () { setCols(c); };
      colsSeg.appendChild(b);
    });
    $("slot-cols").appendChild(colsSeg);

    // 筛选
    var f = document.createElement("select");
    [["all", "全部"], ["pending", "未审阅"], ["keep", "已保留"], ["reject", "不保留"], ["image", "仅图片"], ["video", "仅视频"]]
      .forEach(function (p) {
        var o = document.createElement("option");
        o.value = p[0]; o.textContent = p[1];
        f.appendChild(o);
      });
    f.onchange = function () { S.filter = f.value; reload(); };
    $("slot-filter").appendChild(f);

    var who = $("who");
    if (who) who.textContent = "当前用户: " + S.user;
    updateStat();

    updateParentBtn();
    updateQualityBtn();
    if (S.mode === "download" && el.dlBar) el.dlBar.style.display = "flex";
    if (el.finishBtn) el.finishBtn.onclick = finishReview;
  }

  function setCols(c) {
    S.cols = c;
    try { localStorage.setItem("mr_cols", String(c)); } catch (e) {}
    Array.prototype.forEach.call(document.querySelectorAll("#cols-seg button"), function (b) {
      b.classList.toggle("on", parseInt(b.dataset.cols, 10) === c);
    });
    clearPool();
    measure();
    render();
  }

  // ---------- 目录树 ----------
  function loadTree(parentId) {
    return api("/api/folders?parent=" + parentId).then(function (d) {
      S.treeChildren.set(parentId, d.folders || []);
      return d.folders || [];
    });
  }

  function renderTree() {
    var frag = document.createDocumentFragment();
    var roots = S.treeChildren.get(0) || [];
    if (roots.length === 0) {
      var d = document.createElement("div");
      d.style.cssText = "padding:14px;color:#8f959e;font-size:12px";
      d.textContent = "尚未扫描任何目录";
      frag.appendChild(d);
    }
    roots.forEach(function (n) { frag.appendChild(treeRow(n, 0)); });
    el.tree.innerHTML = "";
    el.tree.appendChild(frag);
  }

  function treeRow(n, depth) {
    var box = document.createElement("div");
    var row = document.createElement("div");
    row.className = "tree-row" + (n.id === S.folderId ? " active" : "") +
      (n.total > 0 && n.pending === 0 ? " done" : "");
    row.style.paddingLeft = (4 + depth * 14) + "px";
    row.dataset.id = n.id;

    var tw = document.createElement("span");
    tw.className = "tw";
    tw.textContent = n.hasChildren ? (S.expanded.has(n.id) ? "▾" : "▸") : "";
    tw.onclick = function (e) {
      e.stopPropagation();
      if (!n.hasChildren) return;
      if (S.expanded.has(n.id)) { S.expanded.delete(n.id); renderTree(); }
      else {
        loadTree(n.id).then(function () { S.expanded.add(n.id); renderTree(); });
      }
    };
    row.appendChild(tw);

    var tn = document.createElement("span");
    tn.className = "tn";
    tn.textContent = n.name;
    tn.title = n.relPath + "  (" + n.keep + " 保留 / " + n.total + " 总数)" +
      (n.claimedBy ? "  ·  " + (n.claimedBy === S.user ? "你" : n.claimedBy) + " 正在审阅" : "");
    row.appendChild(tn);

    // 认领人紧跟目录名（放在 keep/total 徽标之前），长目录名先让 .tn 缩 ——
    // 原来放在行尾的最右边，侧栏一窄就被忽略掉了。
    // 自己认领的显示「我」，避免把自己的名字误读成"别人在审"。
    if (n.claimedBy) {
      var who = (n.claimedBy === S.user) ? "我" : n.claimedBy;
      var ow = document.createElement("span");
      ow.className = "owner";
      ow.textContent = who;
      ow.title = (n.claimedBy === S.user ? "你" : n.claimedBy) + " 正在审阅";
      row.appendChild(ow);
    }

    var tc = document.createElement("span");
    tc.className = "tc";
    tc.textContent = n.keep + "/" + n.total;
    row.appendChild(tc);

    row.onclick = function () {
      if (n.hasChildren && !S.expanded.has(n.id)) {
        loadTree(n.id).then(function () { S.expanded.add(n.id); renderTree(); });
      }
      selectFolder(n.id);
    };
    box.appendChild(row);

    if (S.expanded.has(n.id)) {
      var kids = S.treeChildren.get(n.id) || [];
      kids.forEach(function (k) { box.appendChild(treeRow(k, depth + 1)); });
    }
    return box;
  }

  function selectFolder(id) {
    S.folderId = id;
    clearPool();
    return api("/api/folder?folder=" + id).then(function (d) {
      var n = d.folder;
      S.folderName = n.name;
      S.folderRel = n.relPath;
      S.selfTotal = n.selfTotal || 0;
      S.pending = n.pending || 0;
      S.claimedBy = n.claimedBy || "";
      el.crumb.textContent = n.name;
      el.crumb.title = n.relPath;
      updateClaimBtn();
      updateFinishBtn();
      var kids = loadTree(id).then(function (list) {
        if (list.length) S.expanded.add(id);
      });
      return kids;
    }).then(function () {
      renderTree();
      updateEmptyHint();
      reload();
    }).catch(function (e) {
      toast("打开目录失败: " + e.message);
    });
  }

  // 父目录（只有子目录、自身没有文件）打开时，右侧提示去左侧选子目录，而不是一片空白。
  function updateEmptyHint() {
    var kids = S.treeChildren.get(S.folderId) || [];
    if (S.selfTotal === 0 && kids.length > 0) {
      el.empty.textContent = "这是父目录，请从左侧选择一个子目录查看里面的文件";
    } else if (S.selfTotal === 0) {
      el.empty.textContent = "这个目录是空的";
    } else {
      el.empty.textContent = "没有符合当前筛选条件的文件";
    }
  }

  function updateClaimBtn() {
    if (!el.claimBtn) return;
    // 三个分支都要复位 disabled —— 只在 else 里复位会留下状态残留，
    // 表现为"按钮点不动"。请求进行中的锁定由 claimBtn.onclick 自己负责。
    el.claimBtn.disabled = false;
    if (!S.claimedBy) {
      el.claimBtn.textContent = "认领此目录";
      el.claimBtn.className = "btn attention";
      el.claimBtn.title = "认领后其他人只能查看，你才有权修改";
    } else if (S.claimedBy === S.user) {
      el.claimBtn.textContent = "释放认领（我）";
      el.claimBtn.className = "btn danger";
      el.claimBtn.title = "你在审阅这个目录，点一下可释放";
    } else {
      el.claimBtn.textContent = S.claimedBy + " 已认领";
      el.claimBtn.className = "btn";
      el.claimBtn.title = "已被 " + S.claimedBy + " 认领，你只能查看";
    }
  }

  // 认领成功后立刻把认领人写进左侧树上对应的那一行，不等 refreshTreeSoon 的 700ms 防抖 ——
  // 用户点完马上就能看见，不用怀疑"是不是没生效"。
  function applyOwnerLocal(folderId, who) {
    var hit = false;
    S.treeChildren.forEach(function (list) {
      (list || []).forEach(function (n) {
        if (String(n.id) === String(folderId)) { n.claimedBy = who; hit = true; }
      });
    });
    if (hit) renderTree();
  }

  // 「完成审阅」按钮：还有未定文件时高亮，并显示剩余张数
  function updateFinishBtn() {
    if (!el.finishBtn) return;
    if (S.pending > 0) {
      el.finishBtn.textContent = "完成审阅 · 还有 " + S.pending + " 张未定";
      el.finishBtn.className = "btn primary";
      el.finishBtn.title = "逐个检查还没定下来的图片";
    } else {
      el.finishBtn.textContent = "完成审阅";
      el.finishBtn.className = "btn";
      el.finishBtn.title = "检查本目录还有没有没定下来的图片";
    }
  }

  function reload() {
    S.items = [];
    S.byId = {};
    S.cursorName = "";
    S.cursorId = 0;
    S.hasMore = true;
    clearPool();
    el.grid.style.height = "0px";
    render();
    return loadMore();
  }

  // 图片地址：quality 开关决定默认给压缩图还是原图。
  // orig=1 是服务端已有的参数（见 handleMedia），不是新的东西。
  function mediaURL(f, orig) {
    return U("/api/media?id=" + f.id + (orig ? "&orig=1" : ""));
  }

  function updateQualityBtn() {
    var b = $("btn-quality");
    if (!b) return;
    var isOrig = S.quality === "orig";
    b.textContent = isOrig ? "画质：原图" : "画质：压缩图";
    b.classList.toggle("on", isOrig);
    b.title = isOrig
      ? "当前用原文件显示（加载慢、看得清）。点一下换成压缩图"
      : "当前用低清压缩图（加载快）。点一下换成原图";
  }

  function mkBadge(txt, cls) {
    var b = document.createElement("span");
    b.className = cls;
    b.textContent = txt;
    return b;
  }

  function updateParentBtn() {
    var b = $("btn-parent");
    if (!b) return;
    b.classList.toggle("on", !!S.withParent);
    b.textContent = S.withParent ? "含父目录 ✓" : "不含父目录";
  }

  function loadMore() {
    if (S.loading || !S.hasMore) return Promise.resolve();
    S.loading = true;
    var url = "/api/files?folder=" + S.folderId + "&limit=" + S.page + "&filter=" + S.filter;
    // 父目录的文件只在第一页带出来（服务端也只在没翻页时追加），避免重复
    if (S.cursorId === 0 && S.withParent) url += "&withParent=1";
    if (S.cursorId > 0) url += "&afterName=" + encodeURIComponent(S.cursorName) + "&afterId=" + S.cursorId;
    return api(url).then(function (d) {
      var arr = d.files || [];
      arr.forEach(function (f) { S.items.push(f); S.byId[f.id] = f; });
      // 翻页游标只能按**本层**的文件算，否则会把父目录的 id/名字当游标，下一页就串了
      var own = arr.filter(function (f) { return !f.fromParent; });
      if (own.length > 0) {
        var last = own[own.length - 1];
        S.cursorName = last.name.toLowerCase();
        S.cursorId = last.id;
      }
      if (own.length < S.page) S.hasMore = false;
      S.loading = false;
      updateStat();
      measure();
      render();
      return arr;
    }).catch(function (e) {
      S.loading = false;
      toast("加载文件失败: " + e.message + netHint(e));
    });
  }

  // ---------- 网格窗口化渲染 ----------
  function measure() {
    var pad = IS_PHONE ? 16 : 24;
    var w = el.wrap.clientWidth - pad;
    if (w <= 0) return;
    if (IS_PHONE) {
      // 手机固定 1 列，且卡片占满可视高度：图片 + 文件名条 + 三个标记按钮
      // 一屏全见，不用滚动才够得到按钮。
      S.cols = 1;
      S.colW = w;
      S.cellH = Math.max(150, el.wrap.clientHeight - pad - S.gap);
      S.rowH = S.cellH + S.gap;
      return;
    }
    S.colW = Math.floor((w - S.gap * (S.cols - 1)) / S.cols);
    var thumbH = Math.round(S.colW * 0.72);
    // 24 = 文件名条高度，S.acksH 与 CSS 的 --acks-h 必须一致
    S.cellH = thumbH + 24 + S.acksH;
    S.rowH = S.cellH + S.gap;
  }

  function clearPool() {
    S.pool.forEach(function (e) { hoverPreview(e, false); e.remove(); });
    S.pool.clear();
  }

  function render() {
    var total = S.items.length;
    var rows = Math.ceil(total / S.cols);
    el.grid.style.height = Math.max(0, rows * S.rowH - S.gap) + "px";
    el.empty.style.display = total === 0 && !S.loading && !S.hasMore ? "block" : "none";
    if (total === 0) { S.visA = 0; S.visB = 0; return; }

    var scrollTop = el.wrap.scrollTop;
    var viewH = el.wrap.clientHeight;
    var startRow = Math.max(0, Math.floor(scrollTop / S.rowH) - 1);
    var endRow = Math.ceil((scrollTop + viewH) / S.rowH) + 1;
    var start = startRow * S.cols;
    var end = Math.min(total, endRow * S.cols);

    // 真正可见的区间（下面那个 start/end 带 ±1 行缓冲，比可见范围大，不能拿来做批量范围）
    var visLastRow = Math.ceil((scrollTop + viewH) / S.rowH);
    S.visA = Math.max(0, Math.floor(scrollTop / S.rowH)) * S.cols;
    S.visB = Math.min(total, visLastRow * S.cols);

    var need = new Set();
    for (var i = start; i < end; i++) need.add(i);

    S.pool.forEach(function (node, idx) {
      if (!need.has(idx)) { hoverPreview(node, false); node.remove(); S.pool.delete(idx); }
    });

    for (var j = start; j < end; j++) {
      var node = S.pool.get(j);
      if (!node) {
        node = createCard(S.items[j], j);
        S.pool.set(j, node);
        el.grid.appendChild(node);
      }
      place(node, j);
    }
  }

  // 当前屏幕上可见的文件（不含窗口化渲染的上下缓冲行）。
  // 批量标记只作用于它 —— 用户看到的范围就是被改的范围。
  function visibleItems() {
    if (!S.items.length) return [];
    return S.items.slice(S.visA || 0, S.visB || 0);
  }

  function visibleIds() {
    return visibleItems().map(function (f) { return f.id; });
  }

  function place(node, idx) {
    var col = idx % S.cols;
    var row = Math.floor(idx / S.cols);
    node.style.left = (col * (S.colW + S.gap)) + "px";
    node.style.top = (row * S.rowH) + "px";
    node.style.width = S.colW + "px";
    node.style.height = S.cellH + "px";
  }

  function createCard(f, idx) {
    var card = document.createElement("div");
    card.className = "card" + (f.decision === 1 ? " keep" : f.decision === 2 ? " reject" : "");
    card.dataset.id = f.id;
    card.dataset.idx = idx;

    var thumb = document.createElement("div");
    thumb.className = "thumb";
    // 卡片高度 = thumb + 24(文件名条) + acksH(按钮条)，见 measure()
    thumb.style.height = (S.cellH - 24 - S.acksH) + "px";

    if (f.kind === 1) {
      var img = document.createElement("img");
      img.loading = "lazy";
      img.decoding = "async";
      // 直接显示原图（/api/media 输出原文件），不再生成缩略图
      if (API_BASE) {
        // 跨域（预览面板）：fetch+blob，绕开 CSP img-src 限制
        var tryImg = function (retry) {
          setImg(img, mediaURL(f, S.quality === "orig") + (retry ? "&r=" + Date.now() : ""), function () {
            if (retry) { img.style.display = "none"; showPh(thumb, "无法预览"); return; }
            setTimeout(function () { tryImg(true); }, 1500);
          });
        };
        tryImg(false);
      } else {
        img.src = mediaURL(f, S.quality === "orig");
        img.onerror = function () {
          if (img.dataset.retried) { img.style.display = "none"; showPh(thumb, "无法预览"); return; }
          img.dataset.retried = "1";
          setTimeout(function () { img.src = mediaURL(f, S.quality === "orig") + "&r=" + Date.now(); }, 1500);
        };
      }
      thumb.appendChild(img);
      if (f.fromParent) thumb.appendChild(mkBadge("父目录", "badge-parent"));
    } else {
      var ph = document.createElement("div");
      ph.className = "ph";
      ph.textContent = "视频";
      thumb.appendChild(ph);
      var badge = document.createElement("span");
      badge.className = "badge-vid";
      badge.textContent = "VIDEO";
      thumb.appendChild(badge);
      // 延迟挂 src：快速滚动时不至于对每个视频都发请求
      card._vidTimer = setTimeout(function () {
        if (!card.isConnected) return;
        var v = document.createElement("video");
        v.muted = true;
        v.preload = "metadata";
        v.playsInline = true;
        v.style.cssText = "width:100%;height:100%;object-fit:contain;background:#000;display:block";
        var attach = function (tries) {
          if (!card.isConnected) return;
          videoSrc(f.id, function (src) {
            if (!card.isConnected) return;
            v.src = src;
            if (ph.parentNode) ph.remove();
            thumb.appendChild(v);
            // 悬停时 src 才刚挂上（延迟 attach / 转码刚完成）→ 立刻补播
            if (card._hoverWant && card.matches(":hover")) {
              card._hoverWant = false;
              v.loop = true;
              var p = v.play();
              if (p && p.catch) p.catch(function () {});
            }
          }, function (d) {
            if (!card.isConnected) return;
            if (d.state === "transcoding") {
              ph.textContent = "转码中 " + (d.pct || 0) + "%";
              if (tries < 900) setTimeout(function () { attach(tries + 1); }, 2000);
            } else if (d.state === "unavailable") {
              ph.textContent = "需转码(缺 ffmpeg)";
            } else {
              ph.textContent = "无法播放";
            }
          });
        };
        attach(0);
      }, 500);
    }
    card.appendChild(thumb);

    var bar = document.createElement("div");
    bar.className = "bar";
    var nm = document.createElement("span");
    nm.className = "nm";
    nm.textContent = f.name;
    nm.title = f.name + "  " + fmtSize(f.size);
    bar.appendChild(nm);
    var sz = document.createElement("span");
    sz.textContent = fmtSize(f.size);
    bar.appendChild(sz);
    card.appendChild(bar);

    var acts = document.createElement("div");
    acts.className = "acts";
    acts.appendChild(mkBtn("✓", "k", f.decision === 1, function (e) { e.stopPropagation(); decide(f.id, 1); }));
    acts.appendChild(mkBtn("○", "mid", false, function (e) { e.stopPropagation(); decide(f.id, 0); }));
    acts.appendChild(mkBtn("✕", "x", f.decision === 2, function (e) { e.stopPropagation(); decide(f.id, 2); }));
    card.appendChild(acts);

    card.onmouseenter = function () {
      S.hoverId = f.id;
      // 悬停视频卡片：停稳 ~350ms 后自动静音循环播放预览（手机无悬停，跳过）
      if (f.kind !== 1 && !IS_PHONE) hoverPreview(card, true);
    };
    card.onmouseleave = function () {
      if (f.kind !== 1) hoverPreview(card, false);
    };
    card.onclick = function (e) {
      if (e.target.closest("button")) return;
      openLightbox(idx);
    };
    return card;
  }

  // 网格里视频卡片的悬停预览：停稳一小段时间就静音循环播放，移开暂停并回到第一帧。
  // src 还没挂上（延迟 attach / 还在转码）时先记 _hoverWant，attach 完成后自动补播。
  function hoverPreview(card, on) {
    if (on) {
      clearTimeout(card._hovTimer);
      card._hovTimer = setTimeout(function () {
        if (!card.isConnected || !card.matches(":hover")) return;
        var v = card.querySelector("video");
        if (v && v.src) {
          v.loop = true;
          var p = v.play();
          if (p && p.catch) p.catch(function () {});
        } else {
          card._hoverWant = true;
        }
      }, 350);
    } else {
      clearTimeout(card._hovTimer);
      card._hoverWant = false;
      var v = card.querySelector("video");
      if (v && !v.paused) {
        v.loop = false;
        v.pause();
        try { v.currentTime = 0; } catch (e) {}
      }
    }
  }

  function mkBtn(txt, cls, on, fn) {
    var b = document.createElement("button");
    b.className = cls + (on ? " on" : "");
    b.textContent = txt;
    b.onclick = fn;
    return b;
  }

  function showPh(thumb, msg) {
    var p = document.createElement("div");
    p.className = "ph";
    p.textContent = msg;
    thumb.appendChild(p);
  }

  // ---------- 标记 ----------
  // 所有落标记的入口都必须先过 guardName（名字没登记就不许改）。
  // decide 返回 Promise<boolean>：true 表示"这张处理完了"（灯箱靠它决定要不要翻页）。
  //
  // 后端返回 {changed, blocked, same, missing} 四个计数：
  //   blocked = 被认领锁挡下（这才是"被他人认领"，也只有它配得上这句提示）
  //   same    = 本来就是这个状态，无需写 —— 高频操作（重复点 ✓、批量圈住的已全标过），
  //             以前被误报成"被他人认领"，让人莫名其妙
  //   missing = 文件不存在 / 写失败
  function decide(id, decision) {
    return new Promise(function (resolve) {
      guardName(function () {
        api("/api/review", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ fileId: id, decision: decision }),
        }).then(function (d) {
          if ((d.blocked || 0) > 0) { toast("该目录已被他人认领，只能查看"); resolve(false); return; }
          // changed=0 且没被锁 = 这张本来就是目标状态 —— 照样翻页，别把人卡在这
          applyLocal(id, decision);
          refreshTreeSoon();
          resolve(true);
        }).catch(function (e) { toast("标记失败: " + e.message); resolve(false); });
      });
    });
  }

  function decideMany(ids, decision) {
    if (!ids.length) return;
    guardName(function () {
      api("/api/review/batch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ fileIds: ids, decision: decision }),
      }).then(function (d) {
        var blocked = d.blocked || 0, changed = d.changed || 0, same = d.same || 0;
        if (blocked > 0 && changed === 0) { toast("该目录已被他人认领，只能查看"); return; }
        // 同一批 ids 必然同目录（认领锁按"文件直接所属目录"判），blocked 要么 0 要么全挡，
        // 所以 blocked==0 时全量 applyLocal 是安全的
        if (blocked === 0) ids.forEach(function (id) { applyLocal(id, decision); });
        if (changed > 0) {
          toast("已更新 " + changed + " 个" + (same > 0 ? "（另有 " + same + " 个本来就是）" : ""));
          refreshTreeSoon();
        } else {
          var label = decision === 1 ? "保留" : decision === 2 ? "不保留" : "未定";
          toast("这几个本来就是「" + label + "」，没有要改的");
        }
      }).catch(function (e) { toast("批量标记失败: " + e.message); });
    });
  }

  function applyLocal(id, decision) {
    var f = S.byId[id];
    if (f) f.decision = decision;
    // 「完成审阅」的临时列表是另一批对象，同样要更新，否则灯箱里三个按钮的高亮不跟着变
    if (S.lbOverride) {
      S.lbOverride.forEach(function (x) { if (String(x.id) === String(id)) x.decision = decision; });
    }
    S.pool.forEach(function (node, idx) {
      if (String(node.dataset.id) === String(id)) {
        node.classList.toggle("keep", decision === 1);
        node.classList.toggle("reject", decision === 2);
        Array.prototype.forEach.call(node.querySelectorAll(".acts button"), function (b) {
          b.classList.toggle("on", (b.classList.contains("k") && decision === 1) ||
            (b.classList.contains("x") && decision === 2));
        });
      }
    });
    updateStat();
    var cur = lbList()[S.lbIndex];
    if (S.lbIndex >= 0 && cur && cur.id === id) updateLbButtons();
  }

  function updateStat() {
    var k = 0, r = 0;
    S.items.forEach(function (f) { if (f.decision === 1) k++; else if (f.decision === 2) r++; });
    var txt = "已载入 " + S.items.length + " 个 · 保留 " + k + " · 不保留 " + r + " · 未定 " + (S.items.length - k - r);
    if (S.hasMore) txt += " · 滚动加载更多";
    // 手机上 #who 是隐藏的，把当前用户名放在侧栏标题上，否则完全看不到自己是谁
    el.stat.textContent = (S.user ? S.user + " · " : "") + txt;
  }

  function refreshTreeSoon() {
    clearTimeout(S.refreshTimer);
    S.refreshTimer = setTimeout(function () {
      loadTree(0).then(function () {
        var jobs = [];
        S.expanded.forEach(function (id) { jobs.push(loadTree(id)); });
        return Promise.all(jobs);
      }).then(renderTree);
      // 顺带刷新当前目录的未定张数（「完成审阅」按钮上的角标）
      api("/api/folder?folder=" + S.folderId).then(function (d) {
        S.pending = d.folder.pending || 0;
        updateFinishBtn();
      }).catch(function () {});
    }, 700);
  }

  // ---------- 灯箱 ----------
  // 灯箱读的列表只有一个入口：lbList()。
  // 常规浏览时是当前目录的 S.items；「完成审阅」时会临时换成那一批未定文件的数组
  // （S.lbOverride），此时不跟着标记结果重算，否则会边过边缩、索引错位。
  function lbList() { return S.lbOverride || S.items; }

  // 缩放：**连续**缩放（不再是固定档位），图片和视频都适用。
  //   - 电脑：滚轮缩放（以鼠标位置为锚点）、放大后按住拖动平移
  //   - 手机：双指捏合缩放、单指拖动平移（同样只在放大后）
  // 回到 100% 即还原，不需要单独的还原按钮。
  var Z = { s: 1, tx: 0, ty: 0, moved: false };
  var ZMIN = 1, ZMAX = 6;

  function zoomScale() { return Z.s; }

  function clampPan() {
    var s = Z.s;
    var maxX = Math.max(0, (el.lbBody.clientWidth * (s - 1)) / 2);
    var maxY = Math.max(0, (el.lbBody.clientHeight * (s - 1)) / 2);
    if (Z.tx > maxX) Z.tx = maxX;
    if (Z.tx < -maxX) Z.tx = -maxX;
    if (Z.ty > maxY) Z.ty = maxY;
    if (Z.ty < -maxY) Z.ty = -maxY;
  }

  function lbMedia() { return el.lbBody.querySelector("img, video"); }

  function applyZoom() {
    var m = lbMedia();
    if (!m) return;
    var s = Z.s;
    if (s <= ZMIN + 0.001) { Z.s = ZMIN; Z.tx = 0; Z.ty = 0; s = ZMIN; }
    clampPan();
    m.style.transform = "translate(" + Z.tx + "px," + Z.ty + "px) scale(" + s + ")";
    updateZoomButtons();
  }

  // setScaleAt 把缩放到 s，并让 (ax, ay) 这个点（相对灯箱中心的坐标）在缩放前后
  // 停在原地 —— 滚轮以鼠标为锚点、捏合以双指中点锚点，靠的都是这个。
  // 元素 transform-origin 是 center，变换顺序是 translate 再 scale，所以
  // 内容点的位置 = tx + p * s；令缩放前后相等即可解出新的 tx。
  function setScaleAt(s, ax, ay) {
    s = Math.max(ZMIN, Math.min(ZMAX, s));
    if (ax === undefined) { ax = 0; ay = 0; }
    var old = Z.s;
    Z.tx = ax - (ax - Z.tx) * (s / old);
    Z.ty = ay - (ay - Z.ty) * (s / old);
    Z.s = s;
    applyZoom();
  }

  // 以灯箱可视区中心为原点的坐标（滚轮/捏合的锚点都用这个坐标系）
  function anchorOf(clientX, clientY) {
    var r = el.lbBody.getBoundingClientRect();
    return { x: clientX - r.left - r.width / 2, y: clientY - r.top - r.height / 2 };
  }

  function zoomBy(factor, clientX, clientY) {
    var a = (clientX === undefined) ? { x: 0, y: 0 } : anchorOf(clientX, clientY);
    setScaleAt(Z.s * factor, a.x, a.y);
  }

  function updateZoomButtons() {
    var has = !!lbMedia();
    if (el.lbZin) el.lbZin.disabled = !has || Z.s >= ZMAX - 0.001;
    if (el.lbZout) el.lbZout.disabled = !has || Z.s <= ZMIN + 0.001;
  }

  function setZoom(i) {  // 兼容旧的调用点（按钮/快捷键走 zoomBy）
    zoomBy(i > 0 ? 1.5 : 1 / 1.5);
  }

  function resetZoom() { Z.s = 1; Z.tx = 0; Z.ty = 0; Z.moved = false; }

  // 打开灯箱时，单张的"看原图"状态默认跟随画质开关；翻页也回到这个默认值
  function defaultLbOrig() { return S.quality === "orig"; }

  function openLightbox(idx) {
    S.lbOverride = null;
    S.lbTitle = "";
    S.lbIndex = idx;
    S.lbOrig = defaultLbOrig();
    renderLb();
    el.lightbox.classList.add("on");
  }

  // 「完成审阅」用：只看这一批文件
  function openLightboxList(list, title) {
    S.lbOverride = list;
    S.lbTitle = title || "";
    S.lbIndex = 0;
    S.lbOrig = defaultLbOrig();
    renderLb();
    el.lightbox.classList.add("on");
  }

  function closeLightbox() {
    el.lightbox.classList.remove("on");
    el.lbBody.innerHTML = "";
    resetZoom();
    S.lbIndex = -1;
    S.lbOverride = null;
    S.lbTitle = "";
    S.lbOrig = false;
  }

  function renderLb() {
    var list = lbList();
    var f = list[S.lbIndex];
    if (!f) return;
    resetZoom();   // 每次切图都会重建 img，缩放必须重置
    el.lbName.textContent = (S.lbTitle ? S.lbTitle + "  ·  " : "") + f.name + "  (" + fmtSize(f.size) + ")";
    el.lbBody.innerHTML = "";
    if (f.kind === 1) {
      var img = document.createElement("img");
      setImg(img, mediaURL(f, S.lbOrig), function () {});
      el.lbBody.appendChild(img);
    } else {
      var box = document.createElement("div");
      box.style.cssText = "height:100%;display:flex;align-items:center;justify-content:center;" +
        "color:#e6e6e6;font-size:14px;text-align:center;padding:24px;line-height:1.7";
      var msg = document.createElement("div");
      msg.textContent = "准备中…";
      box.appendChild(msg);
      el.lbBody.appendChild(box);
      var v = document.createElement("video");
      v.controls = true;
      v.autoplay = true;
      var loadVideo = function () {
        videoSrc(f.id, function (src) {
          // 用户已经翻到别的图/关了灯箱 → 不要把旧视频挂回去
          if (!box.isConnected) return;
          v.src = src;
          // 必须像图片一样直接挂到 .lb-body（定高容器）下：
          // 包在 box 里且清掉 box 高度的话，max-height:100% 相对 auto 父级失效，
          // 视频会按原始尺寸渲染（4K 原片直接撑爆），表现就是"忽大忽小"。
          box.remove();
          el.lbBody.appendChild(v);
          var p = v.play();
          if (p && p.catch) p.catch(function () {});
        }, function (d) {
          if (!box.isConnected) return;
          if (d.state === "transcoding") {
            msg.textContent = "正在转码 " + (d.pct || 0) + "%（" + (d.codec || "HEVC") +
              " 编码浏览器无法直接播放，需先转成 H.264）";
            setTimeout(loadVideo, 1500);
          } else if (d.state === "unavailable") {
            msg.textContent = "这个视频是 " + (d.codec || "HEVC") +
              " 编码，浏览器无法播放；服务器未找到 ffmpeg，不能自动转码。";
          } else {
            msg.textContent = "无法播放：" + (d.msg || "未知原因");
          }
        });
      };
      loadVideo();
    }
    if (el.lbCount) el.lbCount.textContent = (S.lbIndex + 1) + " / " + list.length;
    if (el.lbPrev) el.lbPrev.disabled = S.lbIndex <= 0;
    if (el.lbNext) el.lbNext.disabled = (S.lbIndex >= list.length - 1) && (!!S.lbOverride || !S.hasMore);
    updateOrigBtn();
    updateLbButtons();
    updateZoomButtons();
  }

  // 「看原图」按钮：只有图片有；文案随状态切换。预览版默认，点一下临时换原图。
  function updateOrigBtn() {
    if (!el.lbOrig) return;
    var f = lbList()[S.lbIndex];
    var isImg = !!f && f.kind === 1;
    el.lbOrig.hidden = !isImg;
    if (isImg) el.lbOrig.textContent = S.lbOrig ? "回预览" : "看原图";
  }

  function updateLbButtons() {
    var f = lbList()[S.lbIndex];
    if (!f) return;
    ["k", "x", "c"].forEach(function (c) {
      var b = el.lightbox.querySelector(".lb-left ." + c);
      if (!b) return;
      b.classList.toggle("on", (c === "k" && f.decision === 1) || (c === "x" && f.decision === 2) ||
        (c === "c" && f.decision === 0));
    });
  }

  function lbStep(d) {
    var list = lbList();
    var n = S.lbIndex + d;
    if (n < 0 || n >= list.length) {
      // 常规浏览时，翻到最后一张会顺手把下一页拉进来
      if (!S.lbOverride && n >= list.length && S.hasMore) {
        loadMore().then(function () {
          if (n < S.items.length) { S.lbIndex = n; renderLb(); }
        });
      }
      return;
    }
    S.lbIndex = n;
    S.lbOrig = defaultLbOrig(); // 翻页后回到画质开关的默认值
    renderLb();
    if (S.lbOverride) return;
    var row = Math.floor(n / S.cols);
    var top = row * S.rowH;
    if (top < el.wrap.scrollTop || top + S.rowH > el.wrap.scrollTop + el.wrap.clientHeight) {
      el.wrap.scrollTop = top - 20;
    }
  }

  // 灯箱里标记完自动跳到下一张（只在灯箱，网格卡片不自动跳）。
  // 必须挂在 decide 成功之后，失败时不能乱跳；到末尾就停住，不回头循环。
  function lbDecide(d) {
    var f = lbList()[S.lbIndex];
    if (!f) return;
    guardName(function () {
      decide(f.id, d).then(function (ok) {
        if (!ok) return;
        var list = lbList();
        if (S.lbIndex >= list.length - 1) {
          if (S.lbOverride) {
            toast("已检查完这 " + list.length + " 张");
            closeLightbox();
            reload();
          } else {
            toast("已是最后一张");
          }
          return;
        }
        lbStep(1);
      });
    });
  }

  // ---------- 完成审阅：把还没定下来的图片（含"不确定"，在库里和"没标过"是同一个状态）找出来 ----------
  function finishReview() {
    var fid = S.folderId;
    var acc = [], afterName = "", afterId = 0;
    var page = function () {
      var p = "/api/files?folder=" + fid + "&limit=300&filter=pending";
      if (afterId > 0) p += "&afterName=" + encodeURIComponent(afterName) + "&afterId=" + afterId;
      return api(p).then(function (d) {
        var arr = d.files || [];
        acc = acc.concat(arr);
        if (arr.length >= 300) {
          var last = arr[arr.length - 1];
          afterName = last.name.toLowerCase();
          afterId = last.id;
          return page();
        }
        return acc;
      });
    };
    toast("正在检查未定的图片…");
    page().then(function (list) {
      if (!list.length) {
        S.pending = 0;
        updateFinishBtn();
        toast("本目录已全部标记完成，没有未定的图片 ✓");
        return;
      }
      openLightboxList(list, "完成审阅 · 还有 " + list.length + " 张未定");
    }).catch(function (e) { toast("检查失败: " + e.message + netHint(e)); });
  }

  // 灯箱手势：
  //   · 滚轮（电脑）      → 以鼠标位置为锚点连续缩放
  //   · 双指捏合（手机）  → 以双指中点为锚点连续缩放
  //   · 单指 / 鼠标拖动   → 放大后平移（看不同位置）；没放大时不拦截，避免误触
  function bindLbDrag() {
    var start = null;   // 单指拖动
    var drag = null;    // 鼠标拖动
    var pinch = null;   // 双指捏合

    // ---- 滚轮缩放 ----
    el.lbBody.addEventListener("wheel", function (e) {
      if (!lbMedia()) return;
      e.preventDefault();
      // deltaY 向上（负）为放大；不同浏览器/设备的 delta 量级差很多，先归一再取指数
      var d = e.deltaY;
      if (e.deltaMode === 1) d *= 16;        // 行模式
      else if (e.deltaMode === 2) d *= 100;  // 页模式
      var factor = Math.exp(-Math.max(-120, Math.min(120, d)) * 0.0022);
      zoomBy(factor, e.clientX, e.clientY);
    }, { passive: false });

    // ---- 触摸：捏合 + 单指拖动 ----
    el.lbBody.addEventListener("touchstart", function (e) {
      if (!lbMedia()) { start = null; pinch = null; return; }
      if (e.touches.length === 2) {
        start = null;
        var t0 = e.touches[0], t1 = e.touches[1];
        var dx = t1.clientX - t0.clientX, dy = t1.clientY - t0.clientY;
        var d = Math.sqrt(dx * dx + dy * dy) || 1;
        pinch = {
          dist: d,
          cx: (t0.clientX + t1.clientX) / 2,
          cy: (t0.clientY + t1.clientY) / 2,
          s: Z.s, tx: Z.tx, ty: Z.ty,
        };
        Z.moved = true;   // 捏合后浏览器补发的 click 要吞掉，别误关灯箱
        return;
      }
      pinch = null;
      if (zoomScale() <= 1 || e.touches.length !== 1) { start = null; return; }
      var t = e.touches[0];
      start = { x: t.clientX, y: t.clientY, tx: Z.tx, ty: Z.ty };
      Z.moved = false;
    }, { passive: true });

    el.lbBody.addEventListener("touchmove", function (e) {
      if (pinch && e.touches.length === 2) {
        var t0 = e.touches[0], t1 = e.touches[1];
        var ddx = t1.clientX - t0.clientX, ddy = t1.clientY - t0.clientY;
        var d = Math.sqrt(ddx * ddx + ddy * ddy) || 1;
        var want = Math.max(ZMIN, Math.min(ZMAX, pinch.s * (d / pinch.dist)));
        // 以捏合中心为锚点：公式同 setScaleAt，但基准是捏合开始时的状态
        var a = anchorOf(pinch.cx, pinch.cy);
        Z.tx = a.x - (a.x - pinch.tx) * (want / pinch.s);
        Z.ty = a.y - (a.y - pinch.ty) * (want / pinch.s);
        Z.s = want;
        applyZoom();
        e.preventDefault();
        return;
      }
      if (!start || e.touches.length !== 1) return;
      var t = e.touches[0];
      var dx = t.clientX - start.x, dy = t.clientY - start.y;
      if (!Z.moved && Math.abs(dx) + Math.abs(dy) < 6) return;   // 抖动手势阈值
      Z.moved = true;
      Z.tx = start.tx + dx;
      Z.ty = start.ty + dy;
      clampPan();
      applyZoom();
      e.preventDefault();
    }, { passive: false });

    el.lbBody.addEventListener("touchend", function () { start = null; pinch = null; }, { passive: true });

    // ---- 鼠标拖动平移（电脑；同样只在放大后）----
    el.lbBody.addEventListener("mousedown", function (e) {
      if (!lbMedia() || zoomScale() <= 1 || e.button !== 0) { drag = null; return; }
      // 点在视频控制条上时让原生控件先处理
      if (e.target && e.target.tagName === "VIDEO" &&
          e.offsetY !== undefined && el.lbBody.querySelector("video") &&
          e.offsetY > e.target.clientHeight - 48) { drag = null; return; }
      drag = { x: e.clientX, y: e.clientY, tx: Z.tx, ty: Z.ty };
      Z.moved = false;   // 真的挪动了才置位，否则单击会被下面的 capture 白白吞掉
      e.preventDefault();
    });

    window.addEventListener("mousemove", function (e) {
      if (!drag) return;
      var ddx = e.clientX - drag.x, ddy = e.clientY - drag.y;
      if (!Z.moved && Math.abs(ddx) + Math.abs(ddy) < 4) return;   // 抖动阈值
      Z.moved = true;
      Z.tx = drag.tx + ddx;
      Z.ty = drag.ty + ddy;
      clampPan();
      applyZoom();
    });

    window.addEventListener("mouseup", function () {
      if (!drag) return;
      drag = null;
      // 松手后浏览器补发的 click 会在下面的 capture 监听里被吞掉（防误关灯箱）
    });

    // 拖动松手后浏览器会补发一次 click。不在 capture 阶段吞掉它，
    // 就会命中灯箱上"点空白关闭"的监听（因为 e.target 会被算成灯箱本体），
    // 表现为"拖一下图，灯箱自己关了"。
    el.lbBody.addEventListener("click", function (e) {
      if (!Z.moved) return;
      Z.moved = false;
      e.stopPropagation();
      e.preventDefault();
    }, true);
  }

  // ---------- 事件绑定 ----------
  function bindEvents() {
    var lbClose = $("lb-close");
    if (lbClose) lbClose.onclick = closeLightbox;
    var mx = $("modal-x");
    if (mx) mx.onclick = closeModal;

    el.wrap.addEventListener("scroll", function () {
      render();
      if (el.wrap.scrollTop + el.wrap.clientHeight > el.grid.offsetHeight - 900) loadMore();
    });
    window.addEventListener("resize", function () { clearPool(); measure(); render(); });

    el.lbPrev.onclick = function () { lbStep(-1); };
    el.lbNext.onclick = function () { lbStep(1); };
    // 左列：标记
    el.lightbox.querySelector(".lb-left .k").onclick = function () { lbDecide(1); };
    el.lightbox.querySelector(".lb-left .x").onclick = function () { lbDecide(2); };
    el.lightbox.querySelector(".lb-left .c").onclick = function () { lbDecide(0); };
    // 右列：缩放
    if (el.lbZin) el.lbZin.onclick = function () { zoomBy(1.5); };
    if (el.lbZout) el.lbZout.onclick = function () { zoomBy(1 / 1.5); };
    if (el.lbOrig) el.lbOrig.onclick = function () { S.lbOrig = !S.lbOrig; renderLb(); };
    // 只有点到灯箱本体的空白处才关（点图片或按钮都不关）
    el.lightbox.addEventListener("click", function (e) { if (e.target === el.lightbox) closeLightbox(); });

    bindLbDrag();

    // 手机版：⋯ 展开批量操作
    var moreBtn = $("btn-more");
    if (moreBtn) moreBtn.onclick = function () { el.toolbar.classList.toggle("more-open"); };

    document.addEventListener("keydown", function (e) {
      // 名字门禁期间吞掉所有快捷键，只认 Enter
      if (el.nameGate && el.nameGate.classList.contains("on")) {
        if (e.key === "Enter") { e.preventDefault(); if (el.nameGateDone) el.nameGateDone(); }
        return;
      }
      if (el.modal.classList.contains("on")) { if (e.key === "Escape") closeModal(); return; }
      if (e.target.tagName === "INPUT" || e.target.tagName === "SELECT") return;

      if (el.lightbox.classList.contains("on")) {
        if (e.key === "Escape") { closeLightbox(); return; }
        if (e.key === "ArrowLeft") { lbStep(-1); return; }
        if (e.key === "ArrowRight") { lbStep(1); return; }
        // 1/2/0 都算「处理完这一张」，翻页交给 lbDecide 在标记成功后自己做
        if (e.key === "1") { lbDecide(1); return; }
        if (e.key === "2") { lbDecide(2); return; }
        if (e.key === "0") { lbDecide(0); return; }
        if (e.key === "+" || e.key === "=") { zoomBy(1.5); return; }
        if (e.key === "-" || e.key === "_") { zoomBy(1 / 1.5); return; }
        if (e.key === " ") {
          var v = el.lbBody.querySelector("video");
          if (v) { v.paused ? v.play() : v.pause(); e.preventDefault(); }
          return;
        }
      }
      if (e.key === "1" && S.hoverId) { decide(S.hoverId, 1); return; }
      if (e.key === "2" && S.hoverId) { decide(S.hoverId, 2); return; }
      if (e.key === "0" && S.hoverId) { decide(S.hoverId, 0); return; }
      if (e.key === "/") { openRootModal(); return; }
      if (e.key === "?") { openHelp(); return; }
    });

    // 三个批量按钮只作用于「当前屏幕上可见的」卡片。
    // 以前用 S.items（已载入的全部，滚动加载过的都算），用户以为只勾了屏上那几张，
    // 实际把滚出视野的也勾上了 —— 而勾上就会进打包下载。
    $("btn-all-keep").onclick = function () {
      var ids = visibleIds();
      if (!ids.length) { toast("当前屏幕没有可见的文件"); return; }
      confirmDo("把当前屏幕上可见的 " + ids.length + " 个文件全部标为「保留」？", function () {
        decideMany(ids, 1);
      });
    };
    $("btn-all-reject").onclick = function () {
      var ids = visibleIds();
      if (!ids.length) { toast("当前屏幕没有可见的文件"); return; }
      confirmDo("把当前屏幕上可见的 " + ids.length + " 个文件全部标为「不保留」？", function () {
        decideMany(ids, 2);
      });
    };
    $("btn-clear").onclick = function () {
      var ids = visibleIds();
      if (!ids.length) { toast("当前屏幕没有可见的文件"); return; }
      confirmDo("清除当前屏幕上可见的 " + ids.length + " 个文件的标记？", function () {
        decideMany(ids, 0);
      });
    };
    // 「清空本目录标记」是独立入口，只作用本层（不含子目录），
    // 与上面三个「本页…」按钮的可见范围语义彻底分开。
    $("btn-clear-folder").onclick = function () {
      guardName(function () {
        // 点的时候现拉一次，保证弹窗里的数字是此刻的，而不是进目录时的旧值
        api("/api/folder?folder=" + S.folderId).then(function (d) {
          var m = d.marked || 0, mm = d.markedMine || 0;
          if (!m) { toast("本目录还没有任何标记"); return; }
          openClearFolderModal(m, Math.max(0, m - mm));
        }).catch(function (e) { toast("读取目录信息失败: " + e.message); });
      });
    };
    // 「本目录全部保留 / 不保留」：作用整个目录的本层（不含子目录），
    // 与上面三个「本页…」按钮的可见范围语义彻底分开。点的时候现拉目录信息，
    // 用真实文件数做确认弹窗，防止目录树没刷新导致误标。
    function folderDecide(decision) {
      guardName(function () {
        api("/api/folder?folder=" + S.folderId).then(function (d) {
          var n = (d.files != null) ? d.files : -1;
          if (n === 0) { toast("本目录没有文件"); return; }
          var label = decision === 1 ? "保留" : "不保留";
          var tip = n > 0
            ? "把本目录全部 " + n + " 个文件标为「" + label + "」？会覆盖已有标记，不可撤销。"
            : "把本目录全部文件标为「" + label + "」？会覆盖已有标记，不可撤销。";
          confirmDo(tip, function () {
            api("/api/review/folder-decision", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ folderId: S.folderId, decision: decision }),
            }).then(function (r) {
              toast("已把 " + (r.changed || 0) + " 个文件标为「" + label + "」");
              // 本层文件就地更新（父层文件服务端没动，别改）；目录树角标跟着刷新
              S.items.forEach(function (f) { if (!f.fromParent) applyLocal(f.id, decision); });
              updateStat();
              refreshTreeSoon();
            }).catch(function (e) { toast(e.message); });
          });
        }).catch(function (e) { toast("读取目录信息失败: " + e.message); });
      });
    }
    if ($("btn-folder-keep")) $("btn-folder-keep").onclick = function () { folderDecide(1); };
    if ($("btn-folder-reject")) $("btn-folder-reject").onclick = function () { folderDecide(2); };
    $("btn-root").onclick = openRootModal;
    $("btn-help").onclick = openHelp;
    if ($("btn-quality")) {
      $("btn-quality").onclick = function () {
        S.quality = S.quality === "orig" ? "preview" : "orig";
        try { localStorage.setItem("mr_quality", S.quality); } catch (e) {}
        updateQualityBtn();
        toast(S.quality === "orig"
          ? "已切到原图：网格和灯箱都用原文件（加载慢、看得清）"
          : "已切到压缩图：网格和灯箱都用转码后的低清图（加载快）");
        reload();
      };
    }
    if ($("btn-parent")) {
      $("btn-parent").onclick = function () {
        S.withParent = !S.withParent;
        try { localStorage.setItem("mr_parent", S.withParent ? "1" : "0"); } catch (e) {}
        updateParentBtn();
        toast(S.withParent ? "已打开：本目录 + 上一层目录的照片一起显示" : "已关闭：只看本目录");
        reload();
      };
    }

    // 认领 / 释放。两个要点：
    //   ① 请求期间把按钮锁死（防双击）—— 否则连点两下会变成"先认领、再释放"，
    //      数据库净效果为零，界面看着就像"点了没反应"。
    //   ② 必须有 .catch —— 原来这里是全项目唯一没有失败反馈的网络调用，
    //      请求一失败就完全静默，跟"点了没反应"一模一样。
    el.claimBtn.onclick = function () {
      if (el.claimBtn.disabled) return;
      if (S.claimedBy && S.claimedBy !== S.user) { toast("该目录已被 " + S.claimedBy + " 认领"); return; }
      var releasing = (S.claimedBy === S.user);
      el.claimBtn.disabled = true;
      var url = releasing ? "/api/claim/release" : "/api/claim";
      var body = releasing ? { folderId: S.folderId }
        : { folderId: S.folderId, user: S.user };
      api(url, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }).then(function () {
        el.claimBtn.disabled = false;
        S.claimedBy = releasing ? "" : S.user;   // 先乐观更新，界面立刻有反应
        updateClaimBtn();
        applyOwnerLocal(S.folderId, S.claimedBy);
        toast(releasing ? "已释放认领" : "已认领，其他人将只能查看");
        afterClaimChange();                      // 再跟服务端对齐一次，以服务端为准
      }).catch(function (e) {
        el.claimBtn.disabled = false;
        toast("认领失败: " + e.message + netHint(e));
      });
    };

    // 下载相关（仅下载页存在这些按钮）
    var b1 = $("btn-zip");
    if (b1) b1.onclick = function () { doZip(""); };
    var b2 = $("btn-zip-sub");
    if (b2) b2.onclick = function () { doZip("&recursive=1"); };
    var b3 = $("btn-zip-all");
    if (b3) b3.onclick = function () { doZip("&scope=all"); };
    var b4 = $("btn-list");
    if (b4) b4.onclick = function () {
      if (!ensureToken()) return;
      location.href = U("/api/export?folder=" + S.folderId + "&recursive=1&t=" + encodeURIComponent(S.token));
    };
    var b5 = $("btn-copy");
    if (b5) b5.onclick = function () { openCopyModal(); };

    bindResync();
  }

  // 由服务端直接复制文件到本机目录：不经过浏览器、不打包，大批量素材最快的路径
  function openCopyModal() {
    if (!ensureToken()) return;
    el.modalTitle.textContent = "导出保留文件到本机目录";
    el.modalBody.innerHTML = "";

    var hint = document.createElement("div");
    hint.className = "hint";
    hint.innerHTML = "服务端直接复制，不经过浏览器、不打包、不压缩。" +
      "<b>只复制，绝不移动或删除你的源文件。</b><br>" +
      "目标里已存在且大小相同的文件会自动跳过，中途断了再点一次就能接着来。";
    el.modalBody.appendChild(hint);

    var scopeWrap = document.createElement("div");
    scopeWrap.style.cssText = "margin:12px 0;font-size:13px";
    scopeWrap.innerHTML = "<span>范围：</span>";
    var scopeSel = document.createElement("select");
    [["", "仅当前目录"], ["recursive", "当前目录 + 子目录"], ["all", "全部保留文件"]]
      .forEach(function (p) {
        var o = document.createElement("option");
        o.value = p[0]; o.textContent = p[1];
        scopeSel.appendChild(o);
      });
    scopeWrap.appendChild(scopeSel);
    el.modalBody.appendChild(scopeWrap);

    var pathLine = document.createElement("div");
    pathLine.style.cssText = "margin:8px 0;font-size:12px;color:#646a73";
    el.modalBody.appendChild(pathLine);

    var mkRow = document.createElement("div");
    mkRow.style.cssText = "display:flex;gap:6px;margin:8px 0";
    var mkInput = document.createElement("input");
    mkInput.type = "text";
    mkInput.placeholder = "在当前位置下新建子目录";
    mkInput.style.flex = "1";
    var mkBtn = document.createElement("button");
    mkBtn.className = "btn";
    mkBtn.textContent = "新建";
    mkRow.appendChild(mkInput);
    mkRow.appendChild(mkBtn);
    el.modalBody.appendChild(mkRow);

    var list = document.createElement("ul");
    list.className = "browse-list";
    el.modalBody.appendChild(list);

    var cur = "";
    var render = function (d) {
      cur = d.path || cur;
      pathLine.textContent = "目标目录: " + (d.path || "（请选择）");
      list.innerHTML = "";
      if (d.path) {
        var up = document.createElement("li");
        up.innerHTML = "<span>.. 返回上级</span>";
        up.onclick = function () { go(d.parent || ""); };
        list.appendChild(up);
      }
      (d.dirs || []).forEach(function (x) {
        var li = document.createElement("li");
        li.innerHTML = "<span></span><span class='p'></span>";
        li.firstChild.textContent = x.name;
        li.lastChild.textContent = x.path;
        li.onclick = function () { go(x.path); };
        list.appendChild(li);
      });
    };
    var go = function (p) {
      api("/api/browse-dest?path=" + encodeURIComponent(p)).then(render).catch(function (e) {
        pathLine.textContent = "读取失败: " + e.message;
      });
    };
    mkBtn.onclick = function () {
      var nm = (mkInput.value || "").trim();
      if (!nm) return;
      if (!cur) { toast("请先选一个磁盘"); return; }
      api("/api/browse-dest?path=" + encodeURIComponent(cur) + "&mkdir=" + encodeURIComponent(nm))
        .then(render).catch(function (e) { toast("新建失败: " + e.message); });
      mkInput.value = "";
    };
    go("");

    var foot = document.createElement("div");
    foot.className = "modal-foot";
    var cancel = document.createElement("button");
    cancel.className = "btn";
    cancel.textContent = "取消";
    cancel.onclick = closeModal;
    var ok = document.createElement("button");
    ok.className = "btn primary";
    ok.textContent = "开始导出";
    ok.onclick = function () {
      if (!cur) { toast("请先选择目标目录"); return; }
      closeModal();
      toast("正在导出，文件较多时请耐心等待…");
      api("/api/copy", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          folderId: S.folderId,
          recursive: scopeSel.value === "recursive",
          scope: scopeSel.value === "all" ? "all" : "",
          dest: cur,
        }),
      }).then(function (d) {
        toast("导出完成 → " + baseOf(d.dest) + "（新复制 " + d.copied + "，跳过 " + d.skipped +
          "，失败 " + d.failed + "，" + d.elapsed + "）");
        if (d.failed) console.warn("copy errors", d.error);
      }).catch(function (e) { toast("导出失败: " + e.message); });
    };
    foot.appendChild(cancel);
    foot.appendChild(ok);
    el.modalBody.appendChild(foot);
    el.modal.classList.add("on");
  }

  function afterClaimChange() {
    api("/api/folder?folder=" + S.folderId).then(function (d) {
      S.claimedBy = d.folder.claimedBy || "";
      updateClaimBtn();
    }).catch(function () {});
    refreshTreeSoon();
  }

  // 就地归零：不 reload()，避免把用户滚到的位置弹回顶部。
  // 还没载入的后续页不用管 —— 下次 loadMore 时会从服务端拿到已清零的值。
  function clearLocalItems() {
    S.items.forEach(function (f) { applyLocal(f.id, 0); });
    updateStat();
  }

  // 服务端给的路径是本机绝对路径（素材根目录、导出目标目录）。
  // 网页上只露目录名 —— 完整磁盘布局属于**本地控制台**的信息，
  // 手机端/下载页上不该看到 D:\某某\某某 这种东西。
  function baseOf(p) {
    var s = String(p || "").replace(/[\\/]+$/, "");
    var i = Math.max(s.lastIndexOf("\\"), s.lastIndexOf("/"));
    return i >= 0 ? s.slice(i + 1) : (s || "-");
  }

  // 目录名是磁盘上的真实名字 —— 素材来源不可控（外包给的盘、网上下载的目录），
  // 一个叫 <img src=x onerror=…> 的目录名，只要拼进 innerHTML 就能在打开弹窗时执行脚本。
  // 凡是要插进 innerHTML 的动态文本，一律先过这个函数。
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  // 「清空本目录标记」的确认弹窗。比 confirmDo 的一行文字重得多，因为不可逆：
  // 写清范围（本层 / 不含子目录）、摊开数字（总数 + 别人标的数）、
  // 并且必须勾选「我确认」才能点「清空」——这是防误触的最后一层。
  function openClearFolderModal(marked, others) {
    el.modalTitle.textContent = "清空本目录标记";
    el.modalBody.innerHTML = "";

    var hint = document.createElement("div");
    hint.className = "hint";
    hint.style.fontSize = "13px";
    hint.innerHTML = "范围：<b>" + esc(S.folderName || "当前目录") + "</b> 这一层" +
      "（<b>不含子目录</b>）。这些文件的标记会被全部抹掉，" +
      "<b style='color:#dc2626'>无法撤销</b>。";
    el.modalBody.appendChild(hint);

    var cnt = document.createElement("div");
    cnt.style.cssText = "margin:12px 0;font-size:13px";
    cnt.innerHTML = "本层共有 <b>" + marked + "</b> 个文件已被标记" +
      (others > 0 ? "，其中 <b style='color:#b45309'>" + others + "</b> 个是别人标的" : "") + "。";
    el.modalBody.appendChild(cnt);

    var lab = document.createElement("label");
    lab.className = "mini";
    // 覆盖 label.mini 的 --text-dim：这里是不可逆操作的最后一层门禁，字要看得清
    lab.style.cssText = "margin:12px 0 2px;cursor:pointer;font-size:13px;color:var(--text);font-weight:600";
    var cb = document.createElement("input");
    cb.type = "checkbox";
    cb.style.cssText = "width:16px;height:16px;flex:none"; // 手机上也好点
    lab.appendChild(cb);
    lab.appendChild(document.createTextNode(" 我确认要清空本目录的全部标记"));
    el.modalBody.appendChild(lab);

    var foot = document.createElement("div");
    foot.className = "modal-foot";
    var cancel = document.createElement("button");
    cancel.className = "btn";
    cancel.textContent = "取消";
    cancel.onclick = closeModal;
    var ok = document.createElement("button");
    ok.className = "btn danger";
    ok.textContent = "清空";
    ok.disabled = true; // 必须勾选，挡住误触
    cb.onchange = function () { ok.disabled = !cb.checked; };
    ok.onclick = function () { clearFolderNow(); };
    foot.appendChild(cancel);
    foot.appendChild(ok);
    el.modalBody.appendChild(foot);
    el.modal.classList.add("on");
  }

  function clearFolderNow() {
    api("/api/review/clear-folder", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ folderId: S.folderId }),
    }).then(function (d) {
      closeModal();
      clearLocalItems();
      toast("已清空本目录 " + d.changed + " 个标记");
      refreshTreeSoon(); // S.pending / 完成审阅角标 / 目录树角标都在这里刷
    }).catch(function (e) {
      closeModal();
      toast("清空失败: " + e.message); // 403 时 e.message 就是服务端那句中文
    });
  }

  function ensureToken() {
    if (!S.token) {
      toast("缺少下载口令，请用带 ?t=口令 的下载页地址打开");
      return false;
    }
    return true;
  }

  // 流式下载必须走导航跳转，不能用 fetch + blob——80G 会在浏览器内存里炸掉
  function doZip(extra) {
    if (!ensureToken()) return;
    if (!S.canDownload) {
      toast("当前页面没有下载权限，请用带口令的下载页地址打开");
      return;
    }
    var url = U("/api/zip?folder=" + S.folderId + "&t=" + encodeURIComponent(S.token) + extra);
    toast("开始打包，浏览器会接管下载…");
    location.href = url;
  }

  // ---------- 根目录选择 ----------
  function openRootModal() {
    el.modalTitle.textContent = "选择素材根目录";
    el.modalBody.innerHTML = "";
    var box = document.createElement("div");
    var hint = document.createElement("div");
    hint.className = "hint";
    hint.innerHTML = "选择本机或局域网可访问的路径。扫描只会读取文件信息，不会移动或复制你的素材。";
    box.appendChild(hint);
    var pathLine = document.createElement("div");
    pathLine.style.cssText = "margin:10px 0;font-size:12px;color:#646a73";
    box.appendChild(pathLine);
    var list = document.createElement("ul");
    list.className = "browse-list";
    box.appendChild(list);
    el.modalBody.appendChild(box);

    var cur = "";
    var render = function (d) {
      pathLine.textContent = "当前位置: " + (d.path || "我的电脑");
      list.innerHTML = "";
      if (d.parent !== undefined && d.path) {
        var up = document.createElement("li");
        up.innerHTML = "<span>.. 返回上级</span>";
        up.onclick = function () { go(d.parent); };
        list.appendChild(up);
      }
      (d.dirs || []).forEach(function (x) {
        var li = document.createElement("li");
        li.innerHTML = "<span></span><span class='p'></span>";
        li.firstChild.textContent = x.name;
        li.lastChild.textContent = x.path;
        li.onclick = function () { go(x.path); };
        list.appendChild(li);
      });
    };
    var go = function (p) {
      cur = p;
      api("/api/browse?path=" + encodeURIComponent(p)).then(render).catch(function (e) {
        pathLine.textContent = "读取失败: " + e.message;
      });
    };
    go("");

    var foot = document.createElement("div");
    foot.className = "modal-foot";
    var ok = document.createElement("button");
    ok.className = "btn primary";
    ok.textContent = "扫描这个目录";
    ok.onclick = function () {
      if (!cur) { toast("请先选一个目录"); return; }
      api("/api/scan", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ root: cur }),
      }).then(function () {
        closeModal();
        toast("开始扫描，进度见顶部提示条");
        S.expanded = new Set([1]);
        S.treeChildren.clear();
        setTimeout(function () { loadTree(0).then(renderTree); }, 800);
      }).catch(function (e) { toast("扫描失败: " + e.message); });
    };
    var cancel = document.createElement("button");
    cancel.className = "btn";
    cancel.textContent = "取消";
    cancel.onclick = closeModal;
    foot.appendChild(cancel);
    foot.appendChild(ok);
    el.modalBody.appendChild(foot);
    el.modal.classList.add("on");
  }

  function openHelp() {
    el.modalTitle.textContent = "快捷键与说明";
    el.modalBody.innerHTML =
      '<div class="hint">' +
      "<p><b>审阅</b>：鼠标移到卡片上，按 <kbd>1</kbd> 保留、<kbd>2</kbd> 不保留、<kbd>0</kbd> 清除。也可以直接点卡片底部的 ✓ / ○ / ✕。</p>" +
      "<p><b>大图</b>：点卡片打开灯箱，<kbd>←</kbd> <kbd>→</kbd> 切换，<kbd>空格</kbd> 播放/暂停视频，<kbd>Esc</kbd> 关闭。</p>" +
      "<p><b>连续审</b>：灯箱里按 <kbd>1</kbd> / <kbd>2</kbd> / <kbd>0</kbd> 标完会自动跳到下一张，最后一张会停住。想往回看用底部的 ‹。</p>" +
      "<p><b>缩放</b>：电脑上<b>滚轮</b>、手机上<b>双指捏合</b>，都能连续缩放灯箱里的图片和视频（100% ~ 600%）；右侧「＋」「－」按钮和 <kbd>+</kbd> / <kbd>-</kbd> 是一档一档步进的。放大以后可以<b>按住鼠标拖动</b>或<b>单指拖动</b>看不同位置；没放大时拖不动（不影响点击翻页）。</p>" +
      "<p><b>画质开关</b>：工具栏上「画质：压缩图 / 原图」决定<b>网格缩略图和灯箱大图默认用哪个</b> —— 压缩图是服务端转码过的低清版（默认短边 1600，加载快），原图就是原始文件（加载慢但看得清）。选定后会记住，下次打开还是它。注意：<b>不管选哪个，打包 ZIP、导出、复制出来的永远是原始文件</b>；灯箱右下角还有个「看原图」按钮，可以对当前这一张临时切换。</p>" +
      "<p><b>完成审阅</b>：整个目录过完之后点它，会自动把还没定下来的（没标过的，以及标成「不确定」的）挑出来，用灯箱再刷一遍。</p>" +
      "<p><b>批量</b>：「本页全部保留 / 本页全部不保留 / 清除本页标记」只作用于<b>当前屏幕上看得见的</b>那几张；滚动加载进来但已经划出屏幕的不受影响。</p>" +
      "<p><b>清空本目录</b>：「清空本目录标记」作用于<b>这一个目录（不含子目录）</b>里的全部文件，保留 / 不保留 / 不确定都会被抹掉，<b>无法撤销</b>；确认弹窗里会写明本层有多少个已被标记、其中几个是别人标的。它和上面那个「清除本页标记」不是一回事 —— 后者只动屏幕上看得见的那几张。</p>" +
      "<p><b>含父目录</b>：默认打开 —— 进一个子目录时，会把<b>上一层目录直属的</b>照片也一起列出来（卡片左上角有绿色「父目录」角标），省得来回切。点工具栏那个按钮可以关掉，只看本目录。这只是<b>浏览范围</b>：标记、打包、导出的口径完全不变。</p>" +
      "<p><b>下载口径</b>：打包 ZIP、导出路径清单、导出到本机目录、以及单文件下载，<b>都只给标记为「保留」的文件</b>。「不保留」和「没标过 / 不确定」的拿不到。</p>" +
      "<p><b>素材被覆盖过就要重标</b>：同一个位置换了文件（大小或修改时间变了），原来那条「保留」会自动作废，避免把没看过的内容打进交付包。</p>" +
      "<p><b>名字</b>：每次打开页面都要登记批注名，不能跳过；同一个页面里不能改名，想换名字刷新即可。名字只用于区分多人，服务关闭后失效。</p>" +
      "<p><kbd>/</kbd> 重新选择素材根目录，<kbd>?</kbd> 打开本说明。</p>" +
      "<p>手机请用启动窗口里打印的局域网地址访问，并且横屏使用：左侧是文件树，右侧一列大图。</p>" +
      "<p><b>控制台</b>：本机/局域网地址、<b>本次的下载口令</b>、服务端实时日志、停止服务都在<b>本地控制台</b>看。命令行版（medreview.exe）直接显示在黑窗口里；无黑窗版（medreview_ui.exe）双击会弹出一个<b>本地桌面窗口</b>（不是网页），里面有这些信息，还有「打开下载页」「停止服务」按钮。</p>" +
      "<p>多人协作：目录被某人认领后，其他人<b>对这个目录里的文件</b>只能查看不能修改（只锁这一层，父目录被认领不影响子目录）。认领人会显示在左侧对应目录名旁边，你自己的会显示成「我」。重复点同一个标记不会再提示「被他人认领」—— 那只是没变化。</p>" +
      "</div>";
    el.modal.classList.add("on");
  }

  function closeModal() { el.modal.classList.remove("on"); }

  function confirmDo(msg, fn) {
    el.modalTitle.textContent = "确认";
    el.modalBody.innerHTML = '<div class="hint" style="font-size:13px">' + msg + "</div>";
    var foot = document.createElement("div");
    foot.className = "modal-foot";
    var no = document.createElement("button");
    no.className = "btn";
    no.textContent = "取消";
    no.onclick = closeModal;
    var yes = document.createElement("button");
    yes.className = "btn primary";
    yes.textContent = "确定";
    yes.onclick = function () { closeModal(); fn(); };
    foot.appendChild(no);
    foot.appendChild(yes);
    el.modalBody.appendChild(foot);
    el.modal.classList.add("on");
  }

  // ---------- 状态与 SSE ----------
  function loadStatus() {
    return api("/api/status").then(function (st) {
      S.canDownload = !!st.canDownload;
      S.folderRel = st.root || "";
      if (st.root) el.crumb.textContent = st.root.split(/[\\/]/).pop() || st.root;
      el.crumb.title = st.root || "未设置";
      updateScan(st.scan);
      return st;
    }).catch(function (e) { toast("无法连接服务: " + e.message + netHint(e)); });
  }

  function updateScan(scan) {
    if (!scan) return;
    if (scan.running) {
      el.scanbar.classList.add("on");
      el.scanbar.textContent = "正在扫描: " + (scan.current || "") + "  ·  已索引 " + scan.files + " 个文件 / " + scan.folders + " 个目录";
    } else if (scan.error) {
      el.scanbar.classList.add("on");
      el.scanbar.textContent = "扫描出错: " + scan.error;
    } else {
      el.scanbar.classList.remove("on");
    }
  }

  function ensureSSE() {
    // 0=CONNECTING 1=OPEN 2=CLOSED
    if (S.es && S.es.readyState !== 2) return;
    connectSSE();
  }

  function connectSSE() {
    if (S.es) { try { S.es.close(); } catch (e) {} }
    var es = new EventSource(U("/api/events"));
    S.es = es;
    es.onmessage = function (ev) {
      var d;
      try { d = JSON.parse(ev.data); } catch (e) { return; }
      if (d.type === "scan") {
        updateScan(d.data);
        if (!d.data.running) {
          S.expanded = new Set([1]);
          loadTree(0).then(renderTree);
          if (S.items.length === 0) reload();
        }
      } else if (d.type === "review") {
        var id = d.data.fileId;
        if (S.byId[id] && S.byId[id].decision !== d.data.decision) applyLocal(id, d.data.decision);
        refreshTreeSoon();
      } else if (d.type === "claim") {
        // 广播里的 user 字段就是"该目录当前的认领人"（释放时为空串，见 api.go 的两处 Broadcast）。
        // 正打开这个目录的人必须立刻跟上 —— 否则树上的名字变了、
        // 工具条的按钮还是旧的，两边说的不是一回事。
        if (String(d.data.folderId) === String(S.folderId)) {
          S.claimedBy = d.data.user || "";
          updateClaimBtn();
        }
        refreshTreeSoon();
      } else if (d.type === "folder-clear") {
        // 别人（或另一个标签页）清空了本目录：就地归零，不重拉列表（重拉会把滚动位置弹回顶部）。
        // 这条事件是幂等的终态描述，重复收到无副作用；即使它被丢掉，
        // 下一次 reload / 返场 resync 也会从服务端重读对齐。
        if (String(d.data.folderId) === String(S.folderId)) clearLocalItems();
        refreshTreeSoon();
      } else if (d.type === "folder-decision") {
        // 别人（或另一个标签页）把整个目录标成了保留/不保留：本层文件就地应用同款标记，
        // 父层文件服务端没动（folder-decision 只作用本层），不能跟着改。
        if (String(d.data.folderId) === String(S.folderId)) {
          S.items.forEach(function (f) { if (!f.fromParent) applyLocal(f.id, d.data.decision); });
          updateStat();
        }
        refreshTreeSoon();
      }
    };
    es.onerror = function () { /* 浏览器会自动重连 */ };
  }

  // 手机切后台会掉 SSE；服务端事件没有 id: / Last-Event-ID，慢订阅者的事件会被直接丢掉
  // （见 events.go），所以漏掉的标记/认领永远不会补发 —— 回到前台必须主动重新同步一次。
  // 30 秒阈值避免每次切窗口都重拉一遍。
  function bindResync() {
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "hidden") { S.hiddenAt = Date.now(); return; }
      if (!S.hiddenAt || Date.now() - S.hiddenAt < 30000) return;
      S.hiddenAt = 0;
      resync();
    });
    window.addEventListener("pageshow", function () {
      if (!S.hiddenAt || Date.now() - S.hiddenAt < 30000) return;
      S.hiddenAt = 0;
      resync();
    });
  }

  function resync() {
    ensureSSE();
    loadStatus();
    refreshTreeSoon();
    api("/api/folder?folder=" + S.folderId).then(function (d) {
      S.pending = d.folder.pending || 0;
      S.claimedBy = d.folder.claimedBy || "";
      updateClaimBtn();
      updateFinishBtn();
    }).catch(function () {});
    // 灯箱或名字门禁开着时不要重载文件列表，
    // 否则 S.items / S.lbIndex / S.lbOverride 全被重置，会把用户正在看的位置弄乱
    var busy = el.lightbox.classList.contains("on") ||
      (el.nameGate && el.nameGate.classList.contains("on"));
    if (!busy) reload();
  }

  window.initApp = init;
})();
