package main

import (
	"context"
	"log"
	"sync"
	"time"

	"medreview/qc"
)

// QCManager 后台跑质量自动检测：扫描目录里的图片，逐张算 qc.QCResult 落库，
// 跨文件比对 pHash 标记重复，完成一张就 SSE 广播，前端就地刷新徽标/脏点框。
//
// 设计原则（与图片预热同构）：
//   - 重新扫目录 / 换根目录会取消上一轮（warmCancel），免得两轮抢 CPU；
//   - 并发受限（sem），不把磁盘 / ffmpeg 打满；
//   - 已检测且 size/mtime/qc_version 未变的文件直接跳过（HasQC），重进目录几乎零成本；
//   - 绝不碰 review.decision，纯辅助。
type QCManager struct {
	svc   *qc.QCService
	store *Store
	hub   *Hub

	mu        sync.Mutex
	parent    context.Context
	cancelAll context.CancelFunc
	libCancel context.CancelFunc // 仅取消上一轮全库扫描
	sem       chan struct{}      // 并发上限
	hwChoice  string             // "y"=QC 解码用显卡（探测链）；"n"=强制 CPU。bat 询问传入

	// dupMu 让「查重复 + 落库」成为原子操作。没有它时，两张相同的图会被并发处理，
	// 各自查"有没有别的同 hash 文件"时对方**都还没落库**，于是两边都查不到、整组漏标。
	// 只锁这两步 DB 操作（很快），耗时的 ffmpeg 解码仍然是并发的。
	dupMu sync.Mutex

	// pending 记录"已经排进队但还没算完"的文件。WarmFolder 每次开目录都会被触发一次，
	// 快速翻目录（或前端轮询）会把同一张图排进队列几十份，全是重复的 ffmpeg 解码。
	// HasQC 只能挡住"已算完"的，挡不住"正在算"的，所以这里再挡一层。
	// 值是**轮次号**：Cancel 只把 epoch 往前推，旧轮次残留的 goroutine 收尾时
	// 不会误删新一轮刚打的标记（否则又退化成重复排队）。
	pendMu  sync.Mutex
	pending map[int64]uint64
	epoch   uint64
}

func NewQCManager(ffmpeg, ffprobe, cacheDir string, store *Store, hub *Hub) *QCManager {
	parent, cancel := context.WithCancel(context.Background())
	return &QCManager{
		svc:       qc.NewQCService(ffmpeg, ffprobe, cacheDir),
		store:     store,
		hub:       hub,
		parent:    parent,
		cancelAll: cancel,
		sem:       make(chan struct{}, 3), // 3 个视频同时 QC（用户 2026-10-01）
		pending:   make(map[int64]uint64),
	}
}

// Cancel 取消所有正在跑的检测轮次（换根目录 / 重新扫描时调用），并重建 parent
// 以便后续的 WarmFolder/WarmAll 能拿到有效的子 context。
func (m *QCManager) Cancel() {
	m.mu.Lock()
	if m.cancelAll != nil {
		m.cancelAll()
	}
	m.parent, m.cancelAll = context.WithCancel(context.Background())
	m.libCancel = nil
	m.mu.Unlock()
	// 取消后旧队列的标记已作废：只把轮次号往前推，让旧 goroutine 的收尾变成空操作。
	// ⚠️ 别在这里重建 pending map —— 那会让旧 goroutine 的 delete 打到新 map 上，
	// 把新一轮刚打的标记抹掉。
	m.pendMu.Lock()
	m.epoch++
	m.pendMu.Unlock()
}

// WarmFolder 对单个目录本层图片跑检测（后台，只受全局 Cancel 影响，不取消其它轮次）。
func (m *QCManager) WarmFolder(folderID int64) {
	if m.svc == nil || !m.svc.Available() {
		return
	}
	files, err := m.store.FolderMedia(folderID)
	if err != nil {
		log.Printf("QC: 列举目录 %d 失败: %v", folderID, err)
		return
	}
	m.mu.Lock()
	ctx, cancel := context.WithCancel(m.parent)
	m.mu.Unlock()
	// 跑完立刻释放 child ctx（vet 盯着的泄漏点：WithCancel 的 cancel 不调用
	// 会一直占着 parent 的子节点）
	m.runAll(ctx, files, func() { cancel() })
}

// WarmKind 按媒体类型跑 QC（kind<=0 = 图片+视频全跑；扫描完成后调用）。
// 新的一轮会取消上一轮全库扫描，但不影响正在跑的 WarmFolder（各自 context 独立）。
// 全部算完后跑**对账**（连拍近似重复 + 镜头污损跨照片确认）——
// 对账要拿全库当参照物，WarmFolder 只算一层，跑了会拿不全。
// 2026-10-01 起启动顺序为「图片转码 → 图片 QC → 视频转码 → 视频 QC」，
// 所以按 kind 分开调用。
func (m *QCManager) WarmKind(kind int) {
	if m.svc == nil || !m.svc.Available() {
		return
	}
	files, err := m.store.AllMedia(kind)
	if err != nil {
		log.Printf("QC: 列举全库媒体失败: %v", err)
		return
	}
	m.mu.Lock()
	if m.libCancel != nil {
		m.libCancel()
	}
	ctx, c := context.WithCancel(m.parent)
	m.libCancel = c
	m.mu.Unlock()
	// 视频 QC 前探测显卡解码管线：可用 → QC 走显卡，CPU 空出来给并行跑的视频转码
	// （分工，用户 2026-10-01 00:41 定）；不可用 → 全软解（回退，检测数值不变）。
	if kind == kindVideo && len(files) > 0 {
		m.svc.ProbeHWAccel(ctx, m.store.absPath(files[0].RelPath), m.hwChoice)
	}
	m.runAll(ctx, files, func() { m.reconcile(ctx) })
}

func (m *QCManager) runAll(ctx context.Context, files []FileItem, onDone func()) {
	var wg sync.WaitGroup
	for _, f := range files {
		select {
		case <-ctx.Done():
			// 取消也别直接 return：等已在飞的收尾（否则 onDone 会在半截结果上对账）
			wg.Wait()
			return
		default:
		}
		// 已算过且未失效 → 跳过
		if ok, _ := m.store.HasQC(f.ID, f.Size, f.MTime); ok {
			continue
		}
		// 本轮已经在队列里 → 别再排一份（见 pending 的注释）。
		// ⚠️ 必须比对轮次号：只看 key 存不存在的话，Cancel 之后上一轮的残留标记
		// 会把新一轮挡在外面（要等旧 goroutine 退出才释放），表现为"重新扫完
		// 有几张图就是没检测"。跨轮次的旧标记直接覆盖掉。
		m.pendMu.Lock()
		ep := m.epoch
		if v, ok := m.pending[f.ID]; ok && v == ep {
			m.pendMu.Unlock()
			continue
		}
		m.pending[f.ID] = ep
		m.pendMu.Unlock()
		// 用 select 抢信号量：直接 `m.sem <- struct{}{}` 是阻塞发送，
		// 本轮被取消时若正卡在发送上，这个 goroutine 就再也醒不过来了（泄漏）。
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			m.pendMu.Lock()
			if v, ok := m.pending[f.ID]; ok && v == ep {
				delete(m.pending, f.ID)
			}
			m.pendMu.Unlock()
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(f FileItem) {
			defer func() {
				// 只删"自己这一轮"打的标记：Cancel 会把 epoch 往前推，
				// 旧轮次的收尾不能把新一轮刚打的标记抹掉。
				m.pendMu.Lock()
				if v, ok := m.pending[f.ID]; ok && v == ep {
					delete(m.pending, f.ID)
				}
				m.pendMu.Unlock()
				<-m.sem
				wg.Done()
			}()
			m.runOne(ctx, f)
		}(f)
	}
	wg.Wait()
	if onDone != nil {
		onDone()
	}
}

func (m *QCManager) runOne(ctx context.Context, f FileItem) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	abs := m.store.absPath(f.RelPath)
	// 视频先抽代表帧再测（thumbnail 滤镜），判据和图片完全一样
	res, err := m.svc.RunMedia(ctx, abs, f.Kind == 2)
	if err != nil {
		log.Printf("QC: 检测 %s 失败: %v", f.RelPath, err)
		return
	}
	if res == nil {
		return // 无 ffmpeg，跳过
	}
	// EXIF 元数据给跨照片对账用（连拍组 / 镜头污点确认）。
	// 视频没有这些概念，留空即不参与分组。
	device, shootKey := "", ""
	if f.Kind == 1 {
		device, shootKey = exifCaptureTags(abs)
	}
	// 重复判定 + 落库必须一起做完，否则并发下会整组漏标（见 dupMu 的注释）
	m.dupMu.Lock()
	if res.DupHash != "" {
		// 重复判定的三道关（用户 2026-10-01 定的语义：只判「连拍的重复」和「副本」，
		// 整体相似但时间不同、机器不同的不算）：
		// ① hash 相同（候选）；② 拍摄时间差 ≤10 秒且设备不冲突；③ 像素确认 ≤5%。
		// 对端时间未知（副本丢 EXIF）时跳过②直接走像素确认 —— 真副本像素差 0%。
		cands, derr := m.store.OtherDupHashCandidates(res.DupHash, f.ID)
		if derr != nil {
			log.Printf("QC: %s 重复候选查询失败: %v", f.RelPath, derr)
		}
		if len(cands) > 0 {
			log.Printf("QC: %s 重复候选 %d 个", f.RelPath, len(cands))
		}
		myTS := parseShootTime(shootKey)
		for _, c := range cands {
			otherTS := parseShootTime(c.ShootKey)
			if device != "" && c.Device != "" && device != c.Device {
				continue // 机器不同
			}
			if myTS != nil && otherTS != nil {
				dt := myTS.Sub(*otherTS)
				if dt < 0 {
					dt = -dt
				}
				if dt > time.Duration(qc.DupTimeWindowSecs)*time.Second {
					continue // 时间不同（同场景不同时刻）
				}
			}
			ratio, perr := m.svc.PixelDiffRatio(ctx, abs, m.store.absPath(c.RelPath), f.Kind == 2)
			if perr != nil {
				log.Printf("QC: %s 重复像素确认失败，跳过: %v", f.RelPath, perr)
				continue
			}
			// 图片 5%；视频 1%（远景主体小，帧差占比天然低，阈值必须收紧）
			thresh := qc.DupPixelDiffThresh
			if f.Kind == 2 {
				thresh = qc.DupPixelDiffThreshVideo
			}
			log.Printf("QC: %s vs %s 像素差异 %.2f%%（阈值 %.0f%%）", f.RelPath, c.RelPath, ratio*100, thresh*100)
			if ratio <= thresh {
				res.Flags |= qc.FlagDup
				break
			}
		}
	}
	serr := m.store.SaveQC(f.ID, res, f.Size, f.MTime, device, shootKey)
	m.dupMu.Unlock()
	if serr != nil {
		log.Printf("QC: 落库 %s 失败: %v", f.RelPath, serr)
		return
	}
	// 广播给前端（前端按 fileId 就地刷新，不重拉列表）
	if m.hub != nil {
		m.hub.Broadcast("qc", map[string]any{
			"fileId": f.ID,
			"qc":     qcInfoFromResult(res),
		})
	}
}

// reconcile 把单张判定用同设备的其他照片互相印证一遍，改动落库并广播。
// 只在 WarmAll 全库算完后跑；拿全库当参照物，局部目录跑会误判。
func (m *QCManager) reconcile(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	// 等所有在飞的算完再取数：WarmAll 的 wg 只覆盖它自己 spawn 的任务，
	// 并发的 WarmFolder 可能还有几张没入库，那会儿对账的参照物就不全了
	// （实测：对账时 clean 参照图还差 3 张没入库）。带超时，卡死也不拖死流程。
	t0 := time.Now()
	for {
		m.pendMu.Lock()
		n := len(m.pending)
		m.pendMu.Unlock()
		if n == 0 || time.Since(t0) > 15*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	rows, err := m.store.AllQCRows()
	if err != nil {
		log.Printf("QC: 对账取数失败: %v", err)
		return
	}
	plan, dupPairs := reconcilePlan(rows)
	if len(plan) == 0 {
		return
	}
	// 连拍簇的 hash 候选对先过像素确认：显著差异占比 > 5% = 主体不同，
	// 从 plan 里撤掉重复位。失败（读图挂）按"不重复"处理 —— 宁漏勿滥。
	for _, pair := range dupPairs {
		memberID, anchorID := pair[1], pair[0]
		nf, ok := plan[memberID]
		if !ok || nf&qc.FlagDup == 0 {
			continue
		}
		mA, e1 := m.store.FileByID(anchorID)
		mB, e2 := m.store.FileByID(memberID)
		if e1 != nil || e2 != nil {
			continue
		}
		ratio, perr := m.svc.PixelDiffRatio(ctx, m.store.absPath(mA.RelPath), m.store.absPath(mB.RelPath), false)
		if perr == nil && ratio > qc.DupPixelDiffThresh {
			log.Printf("QC: 对账像素确认撤销重复 %s（与锚差异 %.1f%%）", mB.Name, ratio*100)
			plan[memberID] = nf &^ qc.FlagDup
		}
	}
	n := 0
	for id, nf := range plan {
		old, err := m.store.UpdateQCFlags(id, nf)
		if err != nil || old == nf {
			continue
		}
		n++
		if m.hub != nil {
			if info := m.store.QCInfoByID(id); info != nil {
				m.hub.Broadcast("qc", map[string]any{"fileId": id, "qc": info})
			}
		}
	}
	if n > 0 {
		log.Printf("QC: 对账修正 %d 个文件的结论（连拍近似重复 / 镜头污点确认）", n)
	}
}

// parseShootTime 把 shootKey（"2006-01-02 15:04:05"）解析成时间。
// 解析不了（空/格式不对）返回 nil —— 调用方把"未知时间"交给像素确认兜底。
func parseShootTime(shootKey string) *time.Time {
	if shootKey == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", shootKey, time.Local)
	if err != nil {
		return nil
	}
	return &t
}

// SetHWChoice 设置 QC 解码的硬件选择（bat 询问传入）："y"=探测显卡（cuda/qsv/d3d11va 链），
// "n"=强制 CPU 软解。必须在 WarmKind 之前调用。
func (m *QCManager) SetHWChoice(choice string) {
	m.mu.Lock()
	m.hwChoice = choice
	m.mu.Unlock()
	if m.svc != nil {
		m.svc.SetHWChoice(choice)
	}
}

// SetMetaStore 给 QC 服务接上持久化通道：硬件探测结论存库，下次启动直接复用。
func (m *QCManager) SetMetaStore(ms qc.MetaStore) {
	if m.svc != nil {
		m.svc.SetMetaStore(ms)
	}
}
