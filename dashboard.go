package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// ---------- 仪表盘增量推送 ----------
//
// 改造前：仪表盘每 1 秒拉一次 /api/dashboard + /api/vtrans-warmup（全量 JSON），
// 页面每次把 6 个区块全部重建 DOM。数据量小但纯属浪费，而且「谁改了什么」看不出来。
//
// 改造后（本次）：后端把仪表盘拆成若干**区块**，任何数据变化只重算受影响的区块，
// 并且**只在内容真的变了**的时候广播那一个区块。页面连接时先收一份全量快照，
// 之后只收增量 —— 这就是「后端改完一个数据就往前端传一个数据」。
//
// 为什么按区块而不是按字段：
// 一次「整目录标记」会同时改 keep/reject/pending 三个字段，它们之间有算术关系
// （pending = files - keep - reject）。按字段推就要在前端重算这套关系，一旦漏一处
// 就算是漏了推其中一个，页面也会显示出自相矛盾的数字。按区块推则天然自洽 ——
// 一个区块一次算清、一次送达。

const (
	SecTotals  = "totals"  // 文件总数 / 保留 / 不保留 / 待审
	SecMedia   = "media"   // 照片 / 视频 / 待处理预览
	SecFolders = "folders" // 目录认领（含僵尸认领）
	SecWorkers = "workers" // 在岗人员 / 正在审阅
	SecQC      = "qc"      // 质量概况
	SecScan    = "scan"    // 扫描进度
	SecVTrans  = "vtrans"  // 转码情况（含逐文件明细）
	SecIssues  = "issues"  // 异常中心：转码失败 / 素材丢失 / 扫描错误 / QC 损坏
	SecHealth  = "health"  // 健康检查
)

// AllSections 顺序即前端渲染顺序，全量快照按这个顺序输出，便于人工比对。
var AllSections = []string{
	SecTotals, SecMedia, SecFolders, SecWorkers, SecQC, SecScan, SecVTrans, SecIssues, SecHealth,
}

// Dashboard 维护仪表盘各区块的最新值，负责「变了才推」。
//
// 并发模型：状态都在 mu 下访问，但 compute 可能要打数据库、甚至 stat 磁盘上的
// 转码产物，所以**一律在锁外算**，算完再拿锁比对 + 写缓存。这样最贵的 vtrans 区块
// 不会挡住 Mark()（那只是一次置位，必须快）。
type Dashboard struct {
	h *Handler

	mu    sync.Mutex
	cache map[string]json.RawMessage // 区块名 -> 最近一次广播出去的内容
	dirty map[string]bool            // 待重算的区块
	stop  chan struct{}
	once  sync.Once

	lastVT time.Time // vtrans 上次重算时间（限流用）
}

// NewDashboard 建一个增量推送器。此时还不开始跑，调用 Start 启动后台循环。
func NewDashboard(h *Handler) *Dashboard {
	return &Dashboard{
		h:     h,
		cache: map[string]json.RawMessage{},
		dirty: map[string]bool{},
		stop:  make(chan struct{}),
	}
}

// Mark 把若干区块标成待重算。**极快**，可以在每个写操作的末尾随手调。
// 重复标记同一区块没有代价：循环里会合并成一次重算。
func (d *Dashboard) Mark(names ...string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	for _, n := range names {
		d.dirty[n] = true
	}
	d.mu.Unlock()
}

// MarkAll 标脏全部区块（换素材目录、重新扫描这类大变动用）。
func (d *Dashboard) MarkAll() { d.Mark(AllSections...) }

// Start 启动后台重算循环。
//
// 节奏是刻意选的：
//   - 每 300ms 醒一次看有没有脏区块。绝大多数时候（没人操作、没转码）它什么都不做，
//     只查一个 map，CPU 占用可以忽略。
//   - workers 必须定时重算，不能只靠 Mark —— 「近 5 分钟有动作」这个窗口会随时间
//     自然把人踢出去，没有事件能触发。所以每 10 秒无条件重算一次（一条 join 查询，很便宜）。
//   - vtrans 反过来要**限流**：它要 stat 每个视频的转码产物、查探测缓存，是所有区块里
//     最贵的；而它的数据本质低频（转码以分钟计），所以最快 1.5 秒才重算一次。
func (d *Dashboard) Start() {
	if d == nil || d.h == nil {
		return
	}
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		work := time.NewTicker(10 * time.Second)
		defer work.Stop()
		// 启动后先全推一次，这样第一个连上来的页面立刻就是完整的。
		d.MarkAll()
		for {
			select {
			case <-d.stop:
				return
			case <-work.C:
				// 时间驱动的区块：与事件无关，必须自己定期重算。
				d.Mark(SecWorkers)
			case <-tick.C:
				d.flush()
			}
		}
	}()
}

// Stop 停掉后台循环（关服务时用，测试里避免 goroutine 泄漏）。
func (d *Dashboard) Stop() {
	if d == nil {
		return
	}
	d.once.Do(func() { close(d.stop) })
}

func (d *Dashboard) takeDirty(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirty[name]
}

func (d *Dashboard) clearDirty(name string) {
	d.mu.Lock()
	delete(d.dirty, name)
	d.mu.Unlock()
}

// flush 重算所有脏区块，只广播内容真正变化了的那些。
func (d *Dashboard) flush() {
	if d == nil || d.h == nil {
		return
	}
	// vtrans 限流：最贵的区块。1.5 秒内不重复算，且**不清 dirty** —— 下一轮会重试，
	// 所以转码进度不会因为限流而丢，只是推得没那么密。
	if d.takeDirty(SecVTrans) {
		d.mu.Lock()
		due := time.Since(d.lastVT) >= 1500*time.Millisecond
		if due {
			d.lastVT = time.Now()
		}
		d.mu.Unlock()
		if due {
			d.emit(SecVTrans)
		}
	}
	for _, name := range AllSections {
		if name == SecVTrans || !d.takeDirty(name) {
			continue
		}
		d.emit(name)
	}
}

// emit 重算一个区块，内容变了才广播。
func (d *Dashboard) emit(name string) {
	v, err := d.h.calcSection(name)
	if err != nil {
		// 一次算错不该让整个推送循环卡住，更不该把错误刷屏 —— 记一行，下轮靠新的
		// Mark 事件重试。（不清 dirty，避免错误期间疯狂重算刷日志。）
		log.Printf("[dashboard] 区块 %s 重算失败: %v", name, err)
		d.clearDirty(name)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("[dashboard] 区块 %s 序列化失败: %v", name, err)
		d.clearDirty(name)
		return
	}
	d.mu.Lock()
	prev, had := d.cache[name]
	changed := !had || string(prev) != string(b)
	if changed {
		d.cache[name] = b
	}
	delete(d.dirty, name)
	d.mu.Unlock()

	if !changed {
		// 算了一遍但和上次一模一样 —— 一个字节都不推。这是本次改造省下来的主要开销：
		// 「在岗人员」这类只有真变化时才动的区块，空转不再产生任何流量。
		return
	}
	var payload any
	if err := json.Unmarshal(b, &payload); err == nil {
		d.h.hub.Broadcast("dash", map[string]any{"section": name, "data": payload})
	}
}

// Full 返回全量快照（区块名 -> 值），用于：
//   - SSE 连接建立时的首帧，让页面不用先 GET 一次 dashboard；
//   - 兜底全量接口 /api/dashboard。
//
// 这里**不消费 dirty**：全量快照的语义是「当前真实值」，与有没有待推的增量无关。
func (d *Dashboard) Full() map[string]any {
	out := map[string]any{}
	for _, name := range AllSections {
		v, err := d.h.calcSection(name)
		if err != nil {
			continue
		}
		out[name] = v
		// 顺手把缓存对齐，避免紧接着的增量因为「没缓存」而多推一帧全量。
		if b, err := json.Marshal(v); err == nil {
			d.mu.Lock()
			d.cache[name] = b
			d.mu.Unlock()
		}
	}
	return out
}

// ---------- 各区块的计算 ----------
//
// 每个 calc* 都是**纯读**：不改库、不触发生成、不启动转码。这是整套推送能安全
// 高频执行的前提 —— 计算区块绝不能有副作用，否则「算一遍」就等于「干一遍活」。

func (h *Handler) calcSection(name string) (any, error) {
	switch name {
	case SecTotals:
		// 走 totalsSnapshotOK 而不是 totalsSnapshot：查询失败要如实报错，
		// 不能把上一秒的缓存当成"刚算出来的真相"推给页面（静默错数字最危险）。
		t, err := h.totalsSnapshotOK()
		if err != nil {
			return nil, err
		}
		return t, nil
	case SecMedia:
		return h.calcMedia()
	case SecFolders:
		return h.calcFolders()
	case SecWorkers:
		return h.calcWorkers()
	case SecQC:
		return h.store.QCStatsAll()
	case SecScan:
		return h.scanner.Progress(), nil
	case SecVTrans:
		return h.calcVTrans()
	case SecIssues:
		return h.calcIssues()
	case SecHealth:
		return h.calcHealth()
	}
	return nil, fmt.Errorf("未知区块 %q", name)
}

func (h *Handler) calcMedia() (any, error) {
	var photo, video, pend, done int
	// 一条 SELECT 算完四个数：以前是四条独立 COUNT(*)，每次都要扫一遍 file 表。
	err := h.store.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN kind=?     AND present=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN kind=?     AND present=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN processed=0 AND present=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN processed=1 AND present=1 THEN 1 ELSE 0 END),0)
		FROM file`, kindImage, kindVideo).Scan(&photo, &video, &pend, &done)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"photo": photo, "video": video,
		"processedPending": pend, "processedDone": done,
	}, nil
}

func (h *Handler) calcFolders() (any, error) {
	var total, claimed int
	if err := h.store.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM folder),
		(SELECT COUNT(*) FROM claim)`).Scan(&total, &claimed); err != nil {
		return nil, err
	}
	// 僵尸认领：认领了但超过 staleClaimMs 没有任何动作的人锁着目录，人已经不在了。
	// 只统计、**不自动释放** —— 释放要人确认（页面有「强制释放」按钮）。自动踢人会在
	// 合法的长休息（喝咖啡、开会）时把别人的目录抢走，那是更糟的体验。
	//
	// 页面要能一键释放，所以这里把 folderId 和认领人一起给出；不给出就没法在
	// 按钮上确定"释放的是哪个目录、谁的名字"，只能靠用户口头核对。
	type staleClaim struct {
		FolderID int64  `json:"folderId"`
		User     string `json:"user"`
		IdleMs   int64  `json:"idleMs"`
	}
	var staleList []staleClaim
	cutoff := time.Now().UnixMilli() - staleClaimMs
	rows, err := h.store.db.Query(
		`SELECT c.folder_id, c.user_name, ? - c.claimed_at FROM claim c
		 WHERE c.claimed_at < ? ORDER BY c.claimed_at LIMIT 100`, cutoff, cutoff)
	if err == nil {
		for rows.Next() {
			var sc staleClaim
			if rows.Scan(&sc.FolderID, &sc.User, &sc.IdleMs) == nil {
				staleList = append(staleList, sc)
			}
		}
		rows.Close()
	}
	// 僵尸不计入"有效认领" —— 它们占着目录但没人干活，计入的话
	// 「已认领/未认领」会让人以为那些目录真的有人在负责。
	active := claimed - len(staleList)
	if active < 0 {
		active = 0
	}
	return map[string]any{
		"total": total, "claimed": active,
		"unclaimed": total - active,
		"stale":     len(staleList),
		"staleList": staleList,
	}, nil
}

func (h *Handler) calcWorkers() (any, error) {
	workers, err := h.store.ActiveWorkers(time.Now().UnixMilli() - 5*60*1000)
	if err != nil {
		return nil, err
	}
	// 在岗细分：count=在岗总人数，reviewing=其中正在审阅（认领了目录）的人数。
	// Worker.Claiming 非空 = 该人认领了某个目录，正在实际审阅；空 = 只是近 5 分钟
	// 有动作、在浏览（见 ActiveWorkers 的并集定义）。
	reviewing := 0
	for i := range workers {
		if workers[i].Claiming != "" {
			reviewing++
		}
	}
	return map[string]any{"count": len(workers), "reviewing": reviewing, "list": workers}, nil
}

// calcVTrans 转码区块。这里是本次改造里最需要克制的地方。
//
// 旧实现的问题：仪表盘每秒调一次 /api/vtrans-warmup，而它每次都 AllVideos + ListAll，
// ListAll 会对每个视频 os.Stat 转码产物、对未探测的还可能 fork ffprobe。
// 「无鉴权端点 + 每秒全库重算」= 一个能反复拖慢服务的口子。
//
// 现在做三件事：
//  1. 端点本身要过下载鉴权（见 handleVTransWarmup）；
//  2. 页面不再每秒来问，改由本循环在区块变脏时算 —— 天然带 1.5 秒限流；
//  3. 页面直接复用 SSE 推来的这一区块，不再单独请求 /api/vtrans-warmup。
//
// 注意这里仍然每次都算（而不是命中缓存就返回）：限流已经由 flush 保证，
// 再叠一层「静止就不算」的缓存反而会让「预热刚结束」这种边界算不准。
// 真正静止的时候，压根没人 Mark 它，flush 根本不会调到这儿。
func (h *Handler) calcVTrans() (any, error) {
	if h.video == nil {
		return WarmupState{}, nil
	}
	st := h.video.Warmup()
	vids, err := h.store.AllVideos()
	if err != nil {
		return nil, err
	}
	st.Files = h.video.ListAll(vids)
	return st, nil
}

// calcIssues 异常中心：把所有「需要人处理」的东西汇到一处。
//
// 之前错误报告只列转码失败，扫描报错、素材被删、QC 判定损坏都只躺在日志里。
// 这里合并成一个列表，每条带 kind/level，页面按 level 排序展示。
func (h *Handler) calcIssues() (any, error) {
	type issue struct {
		Kind   string `json:"kind"`
		Level  string `json:"level"` // error / warn
		Name   string `json:"name"`
		Detail string `json:"detail"`
	}
	out := []issue{}

	// 1) 转码 / 探测失败
	if h.video != nil {
		if vids, err := h.store.AllVideos(); err == nil {
			for _, f := range h.video.ListAll(vids) {
				if f.State != "failed" {
					continue
				}
				out = append(out, issue{
					Kind: "transcode", Level: "error", Name: f.Name,
					Detail: firstNonEmpty(f.Msg, "转码或探测失败"),
				})
			}
		}
	}

	// 2) 素材丢失：库里还记着、磁盘上已经没了。审阅时最容易踩坑，及时报出来。
	// 上限 200 条 —— 全量拉会让这个区块变成第二个性能问题，而 200 条已经足够定位。
	nMissing := 0
	rows, err := h.store.db.Query(
		`SELECT name FROM file WHERE present=0 ORDER BY name LIMIT ?`, missingIssueLimit+1)
	if err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				nMissing++
				if nMissing <= missingIssueLimit {
					out = append(out, issue{Kind: "missing", Level: "error", Name: n, Detail: "文件已不在磁盘上"})
				}
			}
		}
		rows.Close()
	}
	if nMissing > missingIssueLimit {
		out = append(out, issue{
			Kind: "missing", Level: "warn", Name: "…",
			Detail: fmt.Sprintf("丢失文件超过 %d 个，列表已截断", missingIssueLimit),
		})
	}

	// 3) 扫描错误
	if sp := h.scanner.Progress(); sp.Error != "" {
		out = append(out, issue{Kind: "scan", Level: "error", Name: "扫描失败", Detail: sp.Error})
	}

	// 4) QC 判定文件损坏 —— 不是「素材丢了」，是「文件本身可能有问题」。
	if st, err := h.store.QCStatsAll(); err == nil && st.Corrupt > 0 {
		out = append(out, issue{
			Kind: "qc", Level: "warn",
			Name:   fmt.Sprintf("%d 个文件被判定损坏", st.Corrupt),
			Detail: "QC 检测到无法正常解码，见上方「质量概况」",
		})
	}

	return map[string]any{"items": out, "count": len(out)}, nil
}

// calcHealth 健康检查：给脚本化监控用，也顺手显示在页面上。
func (h *Handler) calcHealth() (any, error) {
	dbOK := h.store.db.Ping() == nil
	rootOK := false
	if root := h.store.Root(); root != "" {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			rootOK = true
		}
	}
	// 转码目录能不能写：建一个临时文件再删掉 —— 这是唯一可靠的探测方式，
	// os.Stat 只能证明它存在，证明不了可写（只读盘、权限、占用都会骗过它）。
	cacheWritable := false
	if h.video != nil && h.video.dir != "" {
		if f, err := os.CreateTemp(h.video.dir, ".health-*"); err == nil {
			name := f.Name()
			f.Close()
			os.Remove(name)
			cacheWritable = true
		}
	}
	return map[string]any{
		"db": dbOK, "root": rootOK, "cacheWritable": cacheWritable,
		"ok": dbOK && rootOK,
	}, nil
}

const (
	// staleClaimMs 超过这个时间没有任何动作就算僵尸认领（30 分钟）。
	staleClaimMs = 30 * 60 * 1000
	// 丢失文件在异常中心最多列这么多条。
	missingIssueLimit = 200
)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
