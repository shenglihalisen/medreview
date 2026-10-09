// 灯箱辅助线 —— jsdom 集成验收（真实 DOM + 真实事件）
// 跑法（项目根目录）：
//   set NODE_PATH=<你的 node_modules 路径>     ← 指向装有 jsdom 的目录
//   node _verify_guides.js
//   例如 npm i -D jsdom 后，用项目内的 node_modules：
//     set NODE_PATH=%CD%\node_modules
const fs = require("fs");
const { JSDOM } = require("jsdom");

const PROTO = require("path").join(__dirname, "web_demo", "mobile_portrait_proto.html");
let pass = 0, fail = 0;
const bad = [];
function ok(cond, name, extra) {
  if (cond) { pass++; console.log("  PASS  " + name); }
  else { fail++; bad.push(name + (extra ? "  → " + extra : "")); console.log("  FAIL  " + name + (extra ? "  → " + extra : "")); }
}

const dom = new JSDOM(fs.readFileSync(PROTO, "utf8"), {
  runScripts: "dangerously",
  pretendToBeVisual: true,
  url: "http://localhost:8848/mobile_portrait_proto.html",
});
const { window } = dom;
const doc = window.document;

// jsdom 无布局引擎，offsetWidth/Height 恒为 0；辅助线靠它算 viewBox，故打桩成真实手机尺寸。
// 竖屏手机典型：图区 340 x 455（3:4 竖图）。
Object.defineProperty(window.HTMLElement.prototype, "offsetWidth",  { get() { return this.id === "lbImg" ? 340 : 100; } });
Object.defineProperty(window.HTMLElement.prototype, "offsetHeight", { get() { return this.id === "lbImg" ? 455 : 100; } });
window.HTMLElement.prototype.getBoundingClientRect = function () {
  const w = this.id === "lbImg" ? 340 : 100, h = this.id === "lbImg" ? 455 : 100;
  return { width: w, height: h, top: 0, left: 0, right: w, bottom: h, x: 0, y: 0 };
};

// ⚠️ 必须清空：辅助线状态存 localStorage，不清会让后续用例被上一轮污染
try { window.localStorage.clear(); } catch (e) {}
const $ = (s) => doc.querySelector(s);
const $$ = (s) => Array.from(doc.querySelectorAll(s));
const gSVG = $("#gSVG"), gPanel = $("#gPanel"), tGuide = $("#tGuide"), tDirt = $("#tDirt");
const kinds = () => Array.from(gSVG.children).map((e) => e.tagName);
const kids = () => Array.from(gSVG.children);

console.log("\n=== A. 结构 ===");
ok(!!gSVG, "SVG 覆盖层已注入 .lb-img");
ok(!!gPanel, "辅助线面板存在");
ok($$("#gPanel input[type=checkbox]").length === 6, "面板有 6 个复选框",
   "实际 " + $$("#gPanel input[type=checkbox]").length);
const names = $$("#gPanel .row span:last-child").map((s) => s.textContent);
ok(names.length === 6, "6 种类型都有中文名", JSON.stringify(names));
ok(!!$("#dirtLayer"), "问题框层存在");

console.log("\n=== B. 面板开合 ===");
ok(gPanel.style.display === "none", "初始面板收起");
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
ok(gPanel.style.display === "block", "点「辅助线」按钮展开面板");
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
ok(gPanel.style.display === "none", "再点一次收起");

console.log("\n=== C. 勾选后真画线 ===");
const boxes = $$("#gPanel input[type=checkbox]");
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
boxes[0].checked = true;
boxes[0].dispatchEvent(new window.Event("change", { bubbles: true }));
ok(kids().length === 4, "九宫格 → 4 条线", "实际 " + kids().length);
ok(kinds().every((k) => k === "line"), "全是 line");
ok(gSVG.getAttribute("viewBox") === "0 0 340 455", "viewBox 用图片实际像素",
   gSVG.getAttribute("viewBox"));
ok(tGuide.textContent.indexOf("●") >= 0, "按钮亮起「辅助线 ●」", tGuide.textContent);
ok(tGuide.classList.contains("on"), "按钮有 on 样式");

console.log("\n=== D. 坐标正确（0-100 比例 × 像素）===");
const vlines = kids().filter((e) => parseFloat(e.getAttribute("x1")) > 0)
                     .map((e) => parseFloat(e.getAttribute("x1"))).sort((a, b) => a - b);
const hlines = kids().filter((e) => parseFloat(e.getAttribute("y1")) > 0)
                     .map((e) => parseFloat(e.getAttribute("y1"))).sort((a, b) => a - b);
ok(vlines.length === 2, "九宫格 = 2 竖 + 2 横", "竖" + vlines.length + " 横" + hlines.length);
ok(Math.abs(vlines[0] - 340 / 3) < 0.01, "竖线1 在 1/3 宽 (" + vlines[0].toFixed(2) + ")");
ok(Math.abs(vlines[1] - 340 * 2 / 3) < 0.01, "竖线2 在 2/3 宽 (" + vlines[1].toFixed(2) + ")");
ok(Math.abs(hlines[0] - 455 / 3) < 0.01, "横线1 在 1/3 高 (" + hlines[0].toFixed(2) + ")");
ok(Math.abs(hlines[1] - 455 * 2 / 3) < 0.01, "横线2 在 2/3 高 (" + hlines[1].toFixed(2) + ")");

console.log("\n=== E. 多选叠加 ===");
const turnOn = (i) => { if (!boxes[i].checked) { boxes[i].checked = true; boxes[i].dispatchEvent(new window.Event("change", { bubbles: true })); } };
turnOn(1);
ok(kids().length === 8, "叠黄金分割 → 8 条", "实际 " + kids().length);
turnOn(4);
ok(kinds().filter((k) => k === "circle").length === 1, "叠中心点 → 出现 1 个 circle");
const circ = gSVG.querySelector("circle");
ok(Math.abs(parseFloat(circ.getAttribute("cx")) - 170) < 0.5, "圆心在水平中点", circ.getAttribute("cx"));
ok(Math.abs(parseFloat(circ.getAttribute("cy")) - 227.5) < 0.5, "圆心在垂直中点", circ.getAttribute("cy"));
turnOn(5);
// 补开剩下两种（对角 2 条 + 中心十字 2 条），凑齐全 6 种共 16 个元素
turnOn(2);
turnOn(3);
const spath = gSVG.querySelector("path");
ok(!!spath, "叠黄金螺旋 → 出现 path");
ok(!/NaN|undefined|Infinity/.test(spath.getAttribute("d")), "螺旋路径无 NaN");
const pts = spath.getAttribute("d").trim().split(/(?=[ML])/).map(function (seg) {
  const xy = seg.slice(1).trim().split(/\s+/).map(Number);
  return { x: xy[0], y: xy[1] };
});
const pxs = pts.map(function (q) { return q.x; });
const pys = pts.map(function (q) { return q.y; });
// —— 覆盖度：黄金螺旋现在是「固定标准形状 + 等比缩放居中」——
// 关键：path 存在 <g transform> 里，测试必须**把 transform 作用到采样点上**，
// 否则读到的是标准矩形(1000×618)里的未变换坐标，会误判成"没适配"。
const IMGW = 340, IMGH = 455;
const gWrap = spath.parentNode;                      // 包裹 path 的 <g>
ok(gWrap && gWrap.tagName === "g", "螺旋包在 <g> 里（等比适配层）", gWrap && gWrap.tagName);
const tr = gWrap.getAttribute("transform") || "";
const mt = tr.match(/translate\(([-\d.]+),([-\d.]+)\)\s*scale\(([\d.]+)\)/);
ok(!!mt, "<g> 带 translate+scale 变换", tr);
const gx = parseFloat(mt[1]), gy = parseFloat(mt[2]), gs = parseFloat(mt[3]);
// 采样点变换到实际画幅坐标
const tp = pts.map(function (q) { return { x: q.x * gs + gx, y: q.y * gs + gy }; });
const txs = tp.map(function (q) { return q.x; });
const tys = tp.map(function (q) { return q.y; });
// 1) 等比不变形：scale 只有一个值（x/y 同一系数）→ 已由正则结构保证，再验算跨度比
const spanX = (Math.max(...txs) - Math.min(...txs)), spanY = (Math.max(...tys) - Math.min(...tys));
ok(gs > 0, "缩放系数为正 (scale=" + gs.toFixed(4) + ")");
// 2) 完整可见：变换后所有点都应落在画幅内（meet = 完整放入，不裁边）
const inside = tp.filter(function (q) {
  return q.x >= -1 && q.x <= IMGW + 1 && q.y >= -1 && q.y <= IMGH + 1;
}).length / tp.length;
ok(inside > 0.98, "螺旋完整落在画幅内（meet 适配，不裁边）", (inside * 100).toFixed(0) + "%");
// 3) 居中：应校验**标准矩形**居中（不是螺旋包围盒居中）。
//    螺旋本身不对称（起点在右下角、涡心偏左上），包围盒中心天然偏离画幅中心 —— 正常。
const STDH = 1000 / 1.6180339887;
ok(Math.abs((gx + 1000 * gs / 2) - IMGW / 2) < 1 &&
   Math.abs((gy + STDH * gs / 2) - IMGH / 2) < 1,
   "标准矩形在画幅内居中",
   "矩形中心(" + (gx + 1000 * gs / 2).toFixed(1) + "," + (gy + STDH * gs / 2).toFixed(1) +
   ") 画幅中心(" + IMGW / 2 + "," + IMGH / 2 + ")");
// 4) 占画面比例合理（不能小到看不见，也不能溢出）
const ratio = (spanX * spanY) / (IMGW * IMGH);
ok(ratio > 0.25, "螺旋占画面 ≥25%（不是一小团）", (ratio * 100).toFixed(0) + "%");
// 5) 半径严格递减（对数螺旋性质）——在标准坐标系里验（未变换）
let mono = true, prev = Infinity;
pts.forEach(function (q) {
  const r = Math.hypot(q.x - 1000 * 0.382, q.y - (1000 / 1.6180339887) * 0.382);
  if (r > prev + 0.5) mono = false;
  prev = r;
});
ok(mono, "半径严格递减（对数螺旋性质，涡心=标准矩形黄金点）");
console.log("       6 种全开 → 元素 " + kids().length + " 个 " +
            JSON.stringify(kids().reduce((a, e) => (a[e.tagName] = (a[e.tagName] || 0) + 1, a), {})));
ok(kids().length === 16, "全叠加 = 16 个元素（4+4+2+2+3+1）", "实际 " + kids().length);

console.log("\n=== F. 全关 ===");
const gclear = $(".gclear");
ok(!!gclear, "有「全部关闭」按钮");
gclear.dispatchEvent(new window.Event("click", { bubbles: true }));
ok(kids().length === 0, "全关后 SVG 清空", "实际 " + kids().length);
ok($$("#gPanel input:checked").length === 0, "复选框全部取消勾选");
ok(tGuide.textContent.indexOf("●") < 0, "按钮恢复「辅助线」", tGuide.textContent);

console.log("\n=== G. 灯箱内联动 ===");
$(".thumb").dispatchEvent(new window.Event("click", { bubbles: true }));
ok($("#lightbox").classList.contains("on"), "点缩略图开灯箱");
// 开一张有 QC 的图（FILES[0].qc=true）
ok($$("#dirtLayer .dirtbox").length === 2, "QC 图画出 2 个镜头脏污红框",
   "实际 " + $$("#dirtLayer .dirtbox").length);
ok(tDirt.textContent.indexOf("●") >= 0, "「问题框 ●」亮起（默认可见）");
// 辅助线在切图后应保留（构图工具与看哪张无关）
turnOn(0);
const before = kids().length;
$("#lbNext").dispatchEvent(new window.Event("click", { bubbles: true }));
ok(kids().length === before, "切下一张后辅助线保留");
ok($("#dirtLayer").children.length === 0, "切到无 QC 的图 → 红框消失");
$("#lbPrev").dispatchEvent(new window.Event("click", { bubbles: true }));
ok($$("#dirtLayer .dirtbox").length === 2, "切回有 QC 的图 → 红框回来");

console.log("\n=== H. 问题框开关 ===");
tDirt.dispatchEvent(new window.Event("click", { bubbles: true }));
ok($$("#dirtLayer .dirtbox").length === 0, "点「问题框」隐藏红框");
ok(tDirt.textContent.indexOf("●") < 0, "按钮恢复「问题框」", tDirt.textContent);
tDirt.dispatchEvent(new window.Event("click", { bubbles: true }));
ok($$("#dirtLayer .dirtbox").length === 2, "再点恢复红框");

console.log("\n=== I. 点面板外自动收起 ===");
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
ok(gPanel.style.display === "block", "面板展开");
$("#lightbox").dispatchEvent(new window.Event("click", { bubbles: true, cancelable: true }));
ok(gPanel.style.display === "none", "点面板外收起");
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
tGuide.dispatchEvent(new window.Event("click", { bubbles: true }));
ok(gPanel.style.display === "none", "连点两次按钮 = 收起（不会误开）");

console.log("\n=== J. 偏好持久化 ===");
const saved = window.localStorage.getItem("mr_proto_guides");
ok(!!saved, "已写入 localStorage", String(saved));
const st = JSON.parse(saved);
ok(st.ruleOfThirds === true, "九宫格状态被记住");
// 模拟刷新：拿存的状态重新初始化
const st2 = JSON.parse(window.localStorage.getItem("mr_proto_guides"));
ok(Object.keys(st2).length === 6, "6 种类型都有状态记录", String(Object.keys(st2).length));

console.log("\n=== K. 无横向溢出 ===");
ok(gSVG.getAttribute("viewBox").split(" ")[2] === "340", "viewBox 宽 = 图宽（1:1 不变形）");
ok($("#dirtLayer").style.pointerEvents === "none", "红框层不吃点击（不挡手势）");
ok(window.getComputedStyle(gSVG).pointerEvents === "none", "辅助线层不吃点击（不挡手势）");
ok(window.getComputedStyle(gSVG).overflow === "hidden", "SVG 裁剪到图片矩形（螺旋不甩到黑底上）",
   window.getComputedStyle(gSVG).overflow);

console.log("\n" + "=".repeat(46));
console.log("通过 " + pass + " / 失败 " + fail);
if (bad.length) { console.log("失败项："); bad.forEach((b) => console.log("  - " + b)); }
dom.window.close();
process.exit(fail ? 1 : 0);
