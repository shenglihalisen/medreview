package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"medreview/qc"
)

const (
	kindOther = 0
	kindImage = 1
	kindVideo = 2

	decisionPending = 0
	decisionKeep    = 1
	decisionReject  = 2
)

type FolderNode struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	RelPath     string `json:"relPath"`
	Total       int    `json:"total"`     // 含所有子孙目录
	Keep        int    `json:"keep"`      // 含所有子孙目录
	Reject      int    `json:"reject"`    // 含所有子孙目录
	Pending     int    `json:"pending"`   // 含所有子孙目录
	SelfTotal   int    `json:"selfTotal"` // 仅本层直接文件
	HasChildren bool   `json:"hasChildren"`
	ClaimedBy   string `json:"claimedBy"`
}

type FileItem struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	RelPath  string `json:"relPath"`
	Size     int64  `json:"size"`
	Kind     int    `json:"kind"`
	Decision int    `json:"decision"`
	Reviewer string `json:"reviewer"`
	MTime    int64  `json:"mtime"`
	// FromParent 只在文件列表接口里临时标注：表示这条其实是**上一层目录**直属的文件
	// （「含父目录」开关打开时带出来的）。不入库，也不参与其它任何逻辑。
	FromParent bool `json:"fromParent"`
	// FolderID 只在服务端内部用：SSE 广播要告诉前端"哪个目录里的文件变了"。
	// 以前广播里塞的是 RelPath（字符串），前端拿它和数字 id 比 —— 永远不等，
	// 那条"别人标了这张图"的实时同步等于一直没生效。不下发给前端。
	FolderID int64 `json:"-"`

	// QC 质量自动检测结果（辅助提示，不参与 decision）。前端在图片上画徽标 / 脏点框。
	// 没有检测结果（还没跑 / ffmpeg 缺失）时为 nil。
	QC *QCInfo `json:"qc,omitempty"`
}

// QCInfo 是给前端看的检测结果摘要。Boxes 仅在镜头脏污命中时有值（归一化 0~1 坐标）。
type QCInfo struct {
	Flags    int  `json:"flags"` // 位掩码：bit0=重复 bit1=损坏 bit2=空镜 bit3=镜头脏污 bit4=模糊 bit5=曝光 bit6=噪点
	Dup      bool `json:"dup"`
	Corrupt  bool `json:"corrupt"`
	Blank    bool `json:"blank"`
	Dirt     bool `json:"dirt"`
	Blur     bool `json:"blur"`
	Exposure bool `json:"exposure"`
	Noise    bool `json:"noise"`
	Shake    bool `json:"shake"`
	// 曝光细分：前端徽标直接显示"欠曝"/"过曝"，比笼统的"曝光"有用
	ExpUnder bool     `json:"expUnder,omitempty"`
	ExpOver  bool     `json:"expOver,omitempty"`
	Boxes    []qc.Box `json:"boxes,omitempty"`
}

// qcInfoFromResult 由 qc.QCResult 构造前端 DTO。
// ⚠️ bool 全部从 **Flags 推导**，不从 detail 里的对象推导：
// 对账 pass 清掉某个位时只改 flags 列，detail 里的对象（如 lensDirt）可能还在，
// 若从这里推导就会出现"bool 是 true、位掩码却没有"的自相矛盾。
func qcInfoFromResult(r *qc.QCResult) *QCInfo {
	if r == nil {
		return nil
	}
	info := &QCInfo{
		Flags:    r.Flags,
		Dup:      r.Flags&qc.FlagDup != 0,
		Corrupt:  r.Flags&qc.FlagCorrupted != 0,
		Blank:    r.Flags&qc.FlagBlank != 0,
		Dirt:     r.Flags&qc.FlagLensDirt != 0,
		Blur:     r.Flags&qc.FlagBlur != 0,
		Exposure: r.Flags&qc.FlagExposure != 0,
		Noise:    r.Flags&qc.FlagNoise != 0,
		Shake:    r.Flags&qc.FlagShake != 0,
	}
	if r.LensDirt != nil {
		info.Boxes = r.LensDirt.Boxes
	}
	if r.Exposure != nil {
		info.ExpUnder, info.ExpOver = r.Exposure.Under, r.Exposure.Over
	}
	return info
}

// qcInfoFromDetail 从落库的 JSON 串还原前端 DTO。
func qcInfoFromDetail(detail string) *QCInfo {
	if detail == "" {
		return nil
	}
	var r qc.QCResult
	if err := json.Unmarshal([]byte(detail), &r); err != nil {
		return nil
	}
	return qcInfoFromResult(&r)
}

type Store struct {
	db   *sql.DB
	root string

	// root 会被 POST /api/scan 改写，同时被所有请求 goroutine 读（absPath / Root）。
	// 以前是直接读写裸 string —— 数据竞争：换素材目录那一瞬间，正在处理请求的
	// goroutine 可能读到"新指针 + 旧长度"这种撕值，拼出来的路径是错的
	// （表现为某一个请求突然 404 / 读到莫名其妙的路径）。
	rootMu sync.RWMutex

	mu       sync.Mutex
	statAt   time.Time
	statMap  map[int64]*folderStat
	childSet map[int64]bool
}

type folderStat struct {
	self, keep, reject int
}

func NewStore(db *sql.DB, root string) *Store {
	return &Store{db: db, root: root}
}

func (s *Store) Root() string {
	s.rootMu.RLock()
	defer s.rootMu.RUnlock()
	return s.root
}

// rootLabel 把素材根目录的绝对路径压成「只留目录名」。
//
// 磁盘布局是**本机控制台**的信息：完整路径、口令、日志都只在那儿看。
// 网页（尤其是手机端和下载页）只要知道"这批素材叫啥"就够了 ——
// 所以 /api/status 和 SSE 广播里一律只发目录名，不发绝对路径。
func rootLabel(p string) string {
	if p == "" {
		return ""
	}
	b := filepath.Base(p)
	if b == "" || b == "." || b == string(filepath.Separator) {
		return p
	}
	return b
}

// SetRoot 换素材根目录。只有启动流程和 POST /api/scan 会调它。
func (s *Store) SetRoot(p string) {
	s.rootMu.Lock()
	s.root = p
	s.rootMu.Unlock()
}

// MetaGet 取一条元信息（不存在返回空串）。
func (s *Store) MetaGet(k string) string { return metaGet(s.db, k) }

// MetaSet 写一条元信息（存在就覆盖）。
func (s *Store) MetaSet(k, v string) error { return metaSet(s.db, k, v) }

// absPath 把相对索引路径还原成磁盘绝对路径。
//
// 两个要点：
//
//  1. **出界校验（Zip Slip 防御）**：filepath.Join 会 Clean，rel 里带 ".." 就能直接
//     逃出素材根。目前 rel 的唯一来源是 scanner 扫描时的 filepath.Rel（按定义不含 ..），
//     但 medreview.db 是明文 SQLite、就在 exe 同目录 —— 一旦它被改写（备份还原、
//     被人投放、以后新增写 rel_path 的功能），一个 "../../../Windows/win.ini" 就能
//     读出根外任意文件。所以这里做最后一道校验：拼完再确认仍在根下，
//     不在就退化成根目录本身（宁可读不到，也不给穿越）。
//
//  2. **加读锁**：root 有 rootMu 保护（SetRoot 换目录时会有并发写），Root() 正确加了
//     RLock，这里原来裸读 s.root，等于把已经修好的数据竞争又引了回来 ———
//     这函数是全部文件读取的唯一入口。
func (s *Store) absPath(rel string) string {
	root := s.Root() // 带读锁
	if root == "" {
		return ""
	}
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return root
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	// p 必须在 root 之下。filepath.Rel 返回以 ".." 开头即表示已逃出。
	if r, err := filepath.Rel(root, p); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return root
	}
	return p
}

// loadStats 一次性算出所有目录的自身计数与父子关系，再由调用方做递归汇总。
// 结果缓存 300ms，避免前端高频刷新反复打数据库。
func (s *Store) loadStats() (map[int64]*folderStat, map[int64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statMap != nil && time.Since(s.statAt) < 300*time.Millisecond {
		return s.statMap, s.childSet, nil
	}
	rows, err := s.db.Query(`
		SELECT f.id, f.parent_id,
			COUNT(fi.id) AS total,
			COALESCE(SUM(CASE WHEN r.decision=1 THEN 1 ELSE 0 END),0) AS keep,
			COALESCE(SUM(CASE WHEN r.decision=2 THEN 1 ELSE 0 END),0) AS reject
		FROM folder f
		LEFT JOIN file fi ON fi.folder_id = f.id
		LEFT JOIN review r ON r.file_id = fi.id
		GROUP BY f.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	stats := make(map[int64]*folderStat)
	children := make(map[int64]int)
	parent := make(map[int64]int64)
	ids := make([]int64, 0, 256)
	for rows.Next() {
		var id, pid int64
		var st folderStat
		if err := rows.Scan(&id, &pid, &st.self, &st.keep, &st.reject); err != nil {
			return nil, nil, err
		}
		stats[id] = &st
		parent[id] = pid
		ids = append(ids, id)
	}
	for _, id := range ids {
		if p := parent[id]; p != 0 {
			children[p]++
		}
	}
	hasChild := make(map[int64]bool, len(children))
	for p := range children {
		hasChild[p] = true
	}
	s.statMap = stats
	s.childSet = hasChild
	s.statAt = time.Now()
	return stats, hasChild, nil
}

// Tree 返回指定父目录下的直接子目录（不带汇总）或带递归汇总的整棵树信息。
func (s *Store) SubFolders(parentID int64) ([]FolderNode, error) {
	stats, hasChild, err := s.loadStats()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, name, rel_path FROM folder WHERE parent_id=? ORDER BY name COLLATE NOCASE`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	claims, err := s.allClaims()
	if err != nil {
		return nil, err
	}
	agg := newAggregator(s.db, stats)

	out := []FolderNode{}
	for rows.Next() {
		var n FolderNode
		if err := rows.Scan(&n.ID, &n.Name, &n.RelPath); err != nil {
			return nil, err
		}
		a := agg.get(n.ID)
		// 扫描进行中可能刚插入新目录、还没进统计缓存，这里必须判空
		if st := stats[n.ID]; st != nil {
			n.SelfTotal = st.self
		}
		n.Total = a.self
		n.Keep = a.keep
		n.Reject = a.reject
		n.Pending = a.self - a.keep - a.reject
		n.HasChildren = hasChild[n.ID]
		n.ClaimedBy = claims[n.ID]
		out = append(out, n)
	}
	return out, rows.Err()
}

type aggStat struct{ self, keep, reject int }

// aggregator 一次构建父子表，之后对每个节点做记忆化后序遍历，
// 保证整棵树 O(n) 算完，而不是每个节点重扫一遍全表。
type aggregator struct {
	stats    map[int64]*folderStat
	children map[int64][]int64
	cache    map[int64]aggStat
}

func newAggregator(db *sql.DB, stats map[int64]*folderStat) *aggregator {
	a := &aggregator{stats: stats, children: map[int64][]int64{}, cache: map[int64]aggStat{}}
	rows, err := db.Query(`SELECT id, parent_id FROM folder`)
	if err != nil {
		return a
	}
	defer rows.Close()
	for rows.Next() {
		var id, pid int64
		if err := rows.Scan(&id, &pid); err == nil && pid != 0 {
			a.children[pid] = append(a.children[pid], id)
		}
	}
	return a
}

func (a *aggregator) get(id int64) aggStat {
	if v, ok := a.cache[id]; ok {
		return v
	}
	st := a.stats[id]
	out := aggStat{}
	if st != nil {
		out.self, out.keep, out.reject = st.self, st.keep, st.reject
	}
	for _, c := range a.children[id] {
		sub := a.get(c)
		out.self += sub.self
		out.keep += sub.keep
		out.reject += sub.reject
	}
	a.cache[id] = out
	return out
}

func (s *Store) FolderByID(id int64) (*FolderNode, error) {
	stats, hasChild, err := s.loadStats()
	if err != nil {
		return nil, err
	}
	claims, err := s.allClaims()
	if err != nil {
		return nil, err
	}
	var n FolderNode
	err = s.db.QueryRow(`SELECT id, name, rel_path FROM folder WHERE id=?`, id).Scan(&n.ID, &n.Name, &n.RelPath)
	if err != nil {
		return nil, err
	}
	if st := stats[n.ID]; st != nil {
		n.SelfTotal = st.self
	}
	if a := newAggregator(s.db, stats).get(n.ID); true {
		n.Total = a.self
		n.Keep = a.keep
		n.Reject = a.reject
		n.Pending = a.self - a.keep - a.reject
	}
	n.HasChildren = hasChild[n.ID]
	n.ClaimedBy = claims[n.ID]
	return &n, nil
}

func (s *Store) allClaims() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT folder_id, user_name FROM claim`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]string{}
	for rows.Next() {
		var id int64
		var u string
		if err := rows.Scan(&id, &u); err == nil {
			m[id] = u
		}
	}
	return m, rows.Err()
}

// FilePage 用 keyset 分页，避免深翻页时 OFFSET 变慢。
func (s *Store) FilePage(folderID int64, afterName string, afterID int64, limit int, filter string) ([]FileItem, error) {
	q := `SELECT f.id, f.name, f.rel_path, f.size, f.kind, COALESCE(r.decision,0), COALESCE(r.reviewer,''), f.mtime,
			COALESCE(qc.flags,0), COALESCE(qc.detail,'')
		FROM file f LEFT JOIN review r ON r.file_id = f.id
		LEFT JOIN qc ON qc.file_id = f.id
		WHERE f.folder_id = ?`
	args := []any{folderID}
	if afterID > 0 {
		q += ` AND (f.sort_key > ? OR (f.sort_key = ? AND f.id > ?))`
		args = append(args, afterName, afterName, afterID)
	}
	switch filter {
	case "keep":
		q += ` AND COALESCE(r.decision,0) = 1`
	case "reject":
		q += ` AND COALESCE(r.decision,0) = 2`
	case "pending":
		q += ` AND COALESCE(r.decision,0) = 0`
	case "video":
		q += ` AND f.kind = 2`
	case "image":
		q += ` AND f.kind = 1`
	// 质量筛选（纯视图过滤，与打包口径无关：打包仍然只认 decision=1）
	case "qcdup":
		q += ` AND (COALESCE(qc.flags,0) & ?) > 0`
		args = append(args, qc.FlagDup)
	case "qcissue":
		q += ` AND (COALESCE(qc.flags,0) & ?) > 0`
		args = append(args, qc.FlagIssueMask)
	}
	q += ` ORDER BY f.sort_key, f.id LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		var qcFlags int
		var qcDetail string
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime,
			&qcFlags, &qcDetail); err != nil {
			return nil, err
		}
		it.QC = qcInfoFromDetail(qcDetail)
		if it.QC != nil {
			it.QC.Flags = qcFlags
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ParentIDOf 返回目录的父目录 id（根目录返回 0）。
func (s *Store) ParentIDOf(folderID int64) (int64, error) {
	var pid int64
	err := s.db.QueryRow(`SELECT parent_id FROM folder WHERE id=?`, folderID).Scan(&pid)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func (s *Store) FileByID(id int64) (*FileItem, error) {
	var it FileItem
	err := s.db.QueryRow(`SELECT f.id, f.name, f.rel_path, f.size, f.kind, COALESCE(r.decision,0), COALESCE(r.reviewer,''), f.mtime, f.folder_id
		FROM file f LEFT JOIN review r ON r.file_id = f.id WHERE f.id=?`, id).
		Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime, &it.FolderID)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// AllVideos 返回库里全部视频文件，供启动时的预热（预检 + 预转码）使用。
// 预热不需要审阅状态，所以不 JOIN review。
func (s *Store) AllVideos() ([]FileItem, error) {
	rows, err := s.db.Query(`SELECT f.id, f.name, f.rel_path, f.size, f.kind, 0, '', f.mtime
		FROM file f WHERE f.kind = ? ORDER BY f.id`, kindVideo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FolderImages 返回某个目录层里的全部图片（不含子目录），供进入目录时的
// 图片预览预热使用。预热不需要审阅状态，所以不 JOIN review。
func (s *Store) FolderImages(folderID int64) ([]FileItem, error) {
	rows, err := s.db.Query(`SELECT f.id, f.name, f.rel_path, f.size, f.kind, 0, '', f.mtime
		FROM file f WHERE f.folder_id = ? AND f.kind = ? AND f.present = 1 ORDER BY f.id`, folderID, kindImage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FolderMedia 返回目录本层的全部素材（图片+视频），供质量自动检测用。
// 与 FolderImages 分开：后者只给图片预览预热用，不该把视频也拉去转码预览。
func (s *Store) FolderMedia(folderID int64) ([]FileItem, error) {
	rows, err := s.db.Query(`SELECT f.id, f.name, f.rel_path, f.size, f.kind, 0, '', f.mtime
		FROM file f WHERE f.folder_id = ? AND f.kind IN (?, ?) AND f.present = 1 ORDER BY f.id`,
		folderID, kindImage, kindVideo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// AllMedia 返回库里全部素材（图片+视频），供扫描完成后的全量质量预检。
func (s *Store) AllMedia(kind int) ([]FileItem, error) {
	// kind <= 0 表示全部（图片+视频）；QC 分阶段跑（先图片后视频）时按 kind 取
	rows, err := s.db.Query(`SELECT f.id, f.name, f.rel_path, f.size, f.kind, 0, '', f.mtime
		FROM file f WHERE f.present = 1 AND (? <= 0 OR f.kind = ?) ORDER BY f.id`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetDecision 写入审阅结论。返回是否真的发生了变化（用于广播）。
func (s *Store) SetDecision(fileID int64, decision int, reviewer string) (bool, error) {
	var old int
	err := s.db.QueryRow(`SELECT COALESCE(decision,0) FROM review WHERE file_id=?`, fileID).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && old == decision {
		return false, nil
	}
	_, err = s.db.Exec(`INSERT INTO review(file_id, decision, reviewer, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(file_id) DO UPDATE SET decision=excluded.decision, reviewer=excluded.reviewer, updated_at=excluded.updated_at`,
		fileID, decision, reviewer, time.Now().Unix())
	if err != nil {
		return false, err
	}
	s.invalidate()
	return true, nil
}

// ClearFolderReview 一次性清空某目录「本层」全部文件的审阅记录。
//
// 单条 DELETE 自带原子性，不需要显式事务；RowsAffected 就是权威的清除数量。
// 故意不递归子目录 —— 作用范围与 GET /api/files?folder=N 严格一致。
//
// 为什么是一条语句而不是像 applyDecision 那样逐文件循环：
//  1. N 次往返 → 1 次（几万文件也是毫秒级，有 idx_file_folder）；
//  2. 原子 —— 循环版中途出错会留下"清一半"的目录，而且无从知道清到哪；
//  3. 不会逐文件广播几千条 SSE（订阅者缓冲只有 32 槽，满了直接丢）。
//
// 保留 present=1：present=0 是"上轮扫到、本轮没扫到"的幽灵行，用户既看不到也标不了，
// 算进来会让返回的数字与用户认知不符；也保证了预览计数与删除用的是同一个谓词。
func (s *Store) ClearFolderReview(folderID int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM review
		WHERE file_id IN (SELECT id FROM file WHERE folder_id = ? AND present = 1)`, folderID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	s.invalidate() // keep/reject/self 统计缓存立刻作废，否则目录树角标不更新
	return n, nil
}

// FolderFileCount 统计某目录「本层」（不递归、present=1）的文件总数。
// 给「本目录全部保留 / 不保留」的确认弹窗做预览用。
func (s *Store) FolderFileCount(folderID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM file WHERE folder_id = ? AND present = 1`, folderID).Scan(&n)
	return n, err
}

// SetFolderReview 把某目录「本层」（不递归、present=1）全部文件的标记一次性设为
// decision（1=保留 / 2=不保留），reviewer 记为操作人。
//
// 已有标记（无论谁标的）都会被覆盖 —— 与 ClearFolderReview 的「整目录操作」语义一致；
// 认领锁在 Handler 层把关（同 clear-folder：认领人本人 / 无人认领才放行）。
// 单条 INSERT..SELECT..ON CONFLICT 自带原子性；返回受影响行数。
func (s *Store) SetFolderReview(folderID int64, decision int, reviewer string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO review(file_id, decision, reviewer, updated_at)
		SELECT f.id, ?, ?, ?
		FROM file f WHERE f.folder_id = ? AND f.present = 1
		ON CONFLICT(file_id) DO UPDATE SET
			decision=excluded.decision, reviewer=excluded.reviewer, updated_at=excluded.updated_at`,
		decision, reviewer, time.Now().Unix(), folderID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	s.invalidate() // keep/reject/self 统计缓存立刻作废，否则目录树角标不更新
	return n, nil
}

// FolderMarkCounts 统计某目录「本层」（不递归、present=1）已被标记的文件数，
// 以及其中 reviewer==user 的数量。给「清空本目录标记」的确认弹窗做预览用。
func (s *Store) FolderMarkCounts(folderID int64, user string) (int, int, error) {
	var total, mine int
	err := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN r.reviewer = ? THEN 1 ELSE 0 END), 0)
		FROM file f JOIN review r ON r.file_id = f.id
		WHERE f.folder_id = ? AND f.present = 1`, user, folderID).Scan(&total, &mine)
	return total, mine, err
}

// FolderDescendants 返回某目录及其所有子孙目录 id，打包下载时用。
func (s *Store) FolderDescendants(id int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id, parent_id FROM folder`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := map[int64][]int64{}
	for rows.Next() {
		var c, p int64
		if err := rows.Scan(&c, &p); err != nil {
			return nil, err
		}
		if p != 0 {
			children[p] = append(children[p], c)
		}
	}
	out := []int64{}
	var walk func(int64)
	walk = func(n int64) {
		out = append(out, n)
		for _, c := range children[n] {
			walk(c)
		}
	}
	walk(id)
	return out, nil
}

func (s *Store) KeptFiles(folderIDs []int64) ([]FileItem, error) {
	if len(folderIDs) == 0 {
		return nil, nil
	}
	q := `SELECT f.id, f.name, f.rel_path, f.size, f.kind, r.decision, COALESCE(r.reviewer,''), f.mtime
		FROM file f JOIN review r ON r.file_id = f.id
		WHERE r.decision = 1 AND f.present = 1 AND f.folder_id IN (`
	args := []any{}
	for i, id := range folderIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `) ORDER BY f.folder_id, f.sort_key, f.id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FilesInFolders 返回这些目录「本层」全部文件及其标记状态。
// 与 KeptFiles 的区别：LEFT JOIN review，**未标记的文件也在**（decision=0）——
// 导出清单要列范围内每一个文件和它是否保留，而不只是导出的那部分。
func (s *Store) FilesInFolders(folderIDs []int64) ([]FileItem, error) {
	if len(folderIDs) == 0 {
		return nil, nil
	}
	q := `SELECT f.id, f.name, f.rel_path, f.size, f.kind, COALESCE(r.decision,0), COALESCE(r.reviewer,''), f.mtime
		FROM file f LEFT JOIN review r ON r.file_id = f.id
		WHERE f.present = 1 AND f.folder_id IN (`
	args := []any{}
	for i, id := range folderIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `) ORDER BY f.folder_id, f.sort_key, f.id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileItem{}
	for rows.Next() {
		var it FileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ClaimFolder 认领（或续期）一个目录。
//
// ⚠️ ON CONFLICT 的 DO UPDATE 必须带 `WHERE claim.user_name = excluded.user_name`：
// 没有这个 WHERE 的话，DO UPDATE 会**无条件覆盖**已有认领人 —— 局域网里任何人都能
// 一条 POST 抢走别人的目录（user 名还能随便填成对方的名字），协作语义直接崩掉。
// 带上 WHERE 后，只有「当前无人认领」或「本来就是自己认领的」两种情况能写入。
//
// 返回值：ok=true 表示成功；ok=false 表示这个目录已被他人认领（调用方应回 409）。
func (s *Store) ClaimFolder(folderID int64, user string) (bool, error) {
	// claimed_at 一律用 **Unix 毫秒**。RenewClaims / ForceReleaseStale / 仪表盘的
	// 僵尸判定都按毫秒算（毫秒精度下 30 分钟窗口不会被"同一秒内"这种边界误判）。
	// 以前这里是 Unix() 秒 —— 一旦有个地方按毫秒读，就会算出"1970 年就过期了"的
	// 荒谬结论（或者反过来，永远不过期）。单位必须统一。
	res, err := s.db.Exec(`INSERT INTO claim(folder_id, user_name, claimed_at) VALUES(?,?,?)
		ON CONFLICT(folder_id) DO UPDATE SET claimed_at=excluded.claimed_at
		WHERE claim.user_name = excluded.user_name`,
		folderID, user, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	// 影响行数 0 = WHERE 不成立（被别人占着）
	n, err := res.RowsAffected()
	if err != nil {
		return true, nil // 拿不到行数就当成功，不因为这个误报
	}
	return n > 0, nil
}

func (s *Store) ReleaseFolder(folderID int64, user string) error {
	_, err := s.db.Exec(`DELETE FROM claim WHERE folder_id=? AND (user_name=? OR ?='')`, folderID, user, user)
	return err
}

// RenewClaims 给这个人的所有认领续期（把 claimed_at 推到现在）。
//
// 为什么需要：以前 claim 一旦写入就永久有效，人临时走开（接电话、开会），
// 目录就一直被锁着，别人只能找管理者手动释放。现在"还在活动"就自动保持锁定，
// 真的离开超过 staleClaimMs 才会被算作僵尸认领（可由仪表盘强制释放）。
//
// 只在**确实持有认领**时才写：先问一句"有没有"，没有就不动。
// 这条路径在每个带 X-User 的请求末尾都会走一遍，不能每次都去 UPDATE。
func (s *Store) RenewClaims(user string) {
	if user == "" {
		return
	}
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claim WHERE user_name=?`, user).Scan(&n); err != nil || n == 0 {
		return
	}
	_, _ = s.db.Exec(`UPDATE claim SET claimed_at=? WHERE user_name=?`, time.Now().UnixMilli(), user)
}

// ForceReleaseStale 强制释放一个**僵尸**认领：只有当该目录的认领超过 staleMs
// 没有任何动作时才允许摘掉。
//
// 这是"解锁"和"抢锁"的分界线 —— 普通 ReleaseFolder 只认自己的（或管理员清空），
// 而这里允许摘掉别人的，但仅限它已经死了。如果不卡 staleMs，任何人都能把一个
// 正在被审的目录抢走，整个认领锁就形同虚设。
//
// 返回 (是否释放成功, 原认领人, 错误)。原认领人用于日志与页面提示。
func (s *Store) ForceReleaseStale(folderID int64, staleMs int64) (bool, string, error) {
	cutoff := time.Now().UnixMilli() - staleMs
	var owner string
	err := s.db.QueryRow(
		`SELECT user_name FROM claim WHERE folder_id=? AND claimed_at < ?`, folderID, cutoff).Scan(&owner)
	if err == sql.ErrNoRows {
		return false, "", nil // 没人认领，或者还在活动期内 → 拒绝
	}
	if err != nil {
		return false, "", err
	}
	// DELETE 再带一次 stale 条件：防止"查完到删之间"刚好被人续期，
	// 那样会把一个刚刚活过来的认领误删掉。
	res, err := s.db.Exec(
		`DELETE FROM claim WHERE folder_id=? AND user_name=? AND claimed_at < ?`,
		folderID, owner, cutoff)
	if err != nil {
		return false, owner, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, owner, err
	}
	return n > 0, owner, nil
}

// ClaimOf 返回某个目录的认领人；没人认领时返回空串。
//
// 与 api.go 的 canEdit 的区别：canEdit 是「按文件」查（经 file.folder_id 中转），
// 这里是「按目录」查。整目录操作（如清空本目录标记）只作用于本层时，
// 一个 folder_id 的认领结论是统一的 —— 要么整体被别人认领、要么整体放行 ——
// 所以查一次就够，不必逐文件 canEdit。
func (s *Store) ClaimOf(folderID int64) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT user_name FROM claim WHERE folder_id=?`, folderID).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return owner, err
}

// FolderExists 轻量存在性检查。不用 FolderByID —— 那个还要算整棵子树的聚合，太重。
func (s *Store) FolderExists(id int64) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM folder WHERE id=?`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) invalidate() {
	s.mu.Lock()
	s.statMap = nil
	s.mu.Unlock()
}

// ---------------------------------------------------------------- 质量自动检测（qc）

// QCStat 一个目录里质量标记的汇总，给工具栏/侧栏的角标用。
type QCStat struct {
	Total    int `json:"total"`
	Dup      int `json:"dup"`
	Corrupt  int `json:"corrupt"`
	Blank    int `json:"blank"`
	Dirt     int `json:"dirt"`
	Blur     int `json:"blur"`
	Exposure int `json:"exposure"`
	Noise    int `json:"noise"`
	Shake    int `json:"shake"`
}

// SaveQC 落库单文件检测结果（按 file_id upsert）。size/mtime/qc_version 用于失效判定。
// res 为 nil 时不清库（nil 表示「这次没跑」，别把已有的结论擦掉）。
func (s *Store) SaveQC(fileID int64, res *qc.QCResult, size, mtime int64, device, shootKey string) error {
	detail := ""
	var flags int
	var hash string
	if res != nil {
		detail = res.Detail()
		flags = res.Flags
		hash = res.DupHash
	}
	_, err := s.db.Exec(`INSERT INTO qc(file_id, flags, detail, dup_hash, device, shoot_key, size, mtime, qc_version, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(file_id) DO UPDATE SET
			flags=excluded.flags, detail=excluded.detail, dup_hash=excluded.dup_hash,
			device=excluded.device, shoot_key=excluded.shoot_key,
			size=excluded.size, mtime=excluded.mtime, qc_version=excluded.qc_version, updated_at=excluded.updated_at`,
		fileID, flags, detail, hash, device, shootKey, size, mtime, qc.QCVersion, time.Now().Unix())
	return err
}

// QCRow 对账 pass 用的一行检测结果。
type QCRow struct {
	FileID   int64
	RelPath  string
	Flags    int
	DupHash  string
	Detail   string
	Device   string
	ShootKey string
}

// AllQCRows 全量拉检测结果给对账 pass 用。detail 里带脏点框坐标，
// 几千行也就几 MB，一次拉完比逐条查省事。
func (s *Store) AllQCRows() ([]QCRow, error) {
	rows, err := s.db.Query(`SELECT q.file_id, f.rel_path, q.flags, q.dup_hash, q.detail, q.device, q.shoot_key
		FROM qc q JOIN file f ON f.id = q.file_id WHERE q.qc_version = ?`, qc.QCVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QCRow{}
	for rows.Next() {
		var r QCRow
		if err := rows.Scan(&r.FileID, &r.RelPath, &r.Flags, &r.DupHash, &r.Detail, &r.Device, &r.ShootKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateQCFlags 对账后改某文件的位掩码（不碰 detail —— flags 才是前端 bool 的唯一真相源）。
// 返回旧 flags，方便调用方判断是否真的变了（变了才要广播）。
func (s *Store) UpdateQCFlags(fileID int64, flags int) (int, error) {
	var old int
	err := s.db.QueryRow(`SELECT flags FROM qc WHERE file_id=?`, fileID).Scan(&old)
	if err != nil {
		return 0, err
	}
	if old == flags {
		return old, nil
	}
	_, err = s.db.Exec(`UPDATE qc SET flags=?, updated_at=? WHERE file_id=?`,
		flags, time.Now().Unix(), fileID)
	return old, err
}

// RememberUser 记下这次用到的批注名，最近使用的排最前。
// 空名字和「匿名」不记。超出上限的旧名字顺手清掉。
func (s *Store) RememberUser(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "匿名" {
		return nil
	}
	// 用**毫秒**：秒级精度下，连续操作的两个名字 last_used 会相同，
	// 排序就成了"看心情"（同值行的顺序不确定），列表顺序会莫名其妙地变。
	if _, err := s.db.Exec(`INSERT INTO known_user(name, last_used) VALUES(?,?)
		ON CONFLICT(name) DO UPDATE SET last_used=excluded.last_used`,
		name, time.Now().UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM known_user WHERE name NOT IN (
		SELECT name FROM known_user ORDER BY last_used DESC, rowid DESC LIMIT ?)`, maxRememberedUsers)
	return err
}

// KnownUsers 返回见过的批注名（最近使用的在前）。
// 末尾加 rowid DESC 兜底：万一 last_used 还是撞了，至少顺序是确定的（后插入的在前）。
func (s *Store) KnownUsers() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM known_user ORDER BY last_used DESC, rowid DESC LIMIT ?`, maxRememberedUsers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QCIssue 问题清单里的一行：QC 检出问题的文件。
type QCIssue struct {
	Name     string
	RelPath  string
	Flags    int
	Detail   string
	Decision int
}

// QCIssues 列出这些目录（含树）下所有 QC 检出问题的文件（flags != 0）。
// 给"问题清单 CSV"用 —— 损坏、重复、模糊这些不再只在界面上看，能导出成表。
func (s *Store) QCIssues(folderIDs []int64) ([]QCIssue, error) {
	if len(folderIDs) == 0 {
		return nil, nil
	}
	q := `SELECT f.name, f.rel_path, COALESCE(r.decision,0), qc.flags, qc.detail
		FROM file f
		JOIN qc ON qc.file_id = f.id AND qc.flags != 0 AND qc.qc_version = ?
		LEFT JOIN review r ON r.file_id = f.id
		WHERE f.present = 1 AND f.folder_id IN (`
	args := []any{qc.QCVersion}
	for i, id := range folderIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `) ORDER BY f.folder_id, f.sort_key, f.id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QCIssue{}
	for rows.Next() {
		var it QCIssue
		if err := rows.Scan(&it.Name, &it.RelPath, &it.Decision, &it.Flags, &it.Detail); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// QCInfoByID 读单条检测结果（对账后广播用）。
func (s *Store) QCInfoByID(fileID int64) *QCInfo {
	var detail string
	err := s.db.QueryRow(`SELECT detail FROM qc WHERE file_id=?`, fileID).Scan(&detail)
	if err != nil {
		return nil
	}
	info := qcInfoFromDetail(detail)
	if info != nil {
		var flags int
		_ = s.db.QueryRow(`SELECT flags FROM qc WHERE file_id=?`, fileID).Scan(&flags)
		info.Flags = flags
	}
	return info
}

// DupCand 同 hash 的候选对端（重复判定的候选，不是结论）。
type DupCand struct {
	RelPath  string
	Device   string
	ShootKey string
}

// OtherDupHashCandidates 全库查找「不是自己、且非损坏」的相同 pHash 文件。
// hash 相同只是**候选**：背景相同的连拍（主体不同）、同场景不同时刻的拍摄
// hash 都可能一样。调用方必须再过两道关：
//  1. 时间/设备约束（用户 2026-10-01：整体相似但时间不同、机器不同的不算重复）
//  2. 像素级确认（PixelDiffRatio，显著差异占比 > 5% = 主体不同）
func (s *Store) OtherDupHashCandidates(hash string, exclude int64) ([]DupCand, error) {
	if hash == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT f.rel_path, q.device, q.shoot_key FROM qc q JOIN file f ON f.id = q.file_id
		WHERE q.dup_hash=? AND q.file_id<>? AND (q.flags & ?)=0 LIMIT 20`,
		hash, exclude, qc.FlagCorrupted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DupCand{}
	for rows.Next() {
		var c DupCand
		if err := rows.Scan(&c.RelPath, &c.Device, &c.ShootKey); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// QCStats 统计某目录「本层」（present=1）各质量标记的数量。
func (s *Store) QCStats(folderID int64) (QCStat, error) {
	var st QCStat
	err := s.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0)
		FROM file f JOIN qc ON qc.file_id = f.id
		WHERE f.folder_id=? AND f.present=1`,
		qc.FlagDup, qc.FlagCorrupted, qc.FlagBlank, qc.FlagLensDirt,
		qc.FlagBlur, qc.FlagExposure, qc.FlagNoise, qc.FlagShake, folderID).
		Scan(&st.Total, &st.Dup, &st.Corrupt, &st.Blank, &st.Dirt,
			&st.Blur, &st.Exposure, &st.Noise, &st.Shake)
	return st, err
}

// HasQC 文件是否已有有效（qc_version 匹配）的检测结果。供后台触发时跳过已算过的。
func (s *Store) HasQC(fileID, size, mtime int64) (bool, error) {
	var ver int64
	var sz, mt int64
	err := s.db.QueryRow(`SELECT qc_version, size, mtime FROM qc WHERE file_id=?`, fileID).
		Scan(&ver, &sz, &mt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ver == qc.QCVersion && sz == size && mt == mtime, nil
}

// MarkProcessed 标记某个媒体已生成可浏览预览/转码产物（仪表盘「已处理」用）。
// WHERE processed=0 保证只写一次、幂等，重复调用零副作用。
func (s *Store) MarkProcessed(fileID int64) error {
	_, err := s.db.Exec(`UPDATE file SET processed=1 WHERE id=? AND processed=0`, fileID)
	return err
}

// Worker 表示当前在岗的审阅员。Claiming 为其认领的目录名（未认领则为空串）。
type Worker struct {
	Name     string `json:"name"`
	Claiming string `json:"claiming"`
}

// ActiveWorkers 返回「正在干活」的人：当前认领了目录的人，加上近 cutoffMs 毫秒内
// 有过动作（known_user.last_used 被刷新）的人，两者取并集、按名字去重。
func (s *Store) ActiveWorkers(cutoffMs int64) ([]Worker, error) {
	merged := map[string]*Worker{}
	add := func(name, claiming string) {
		if name == "" || name == "匿名" {
			return
		}
		if w, ok := merged[name]; ok {
			if claiming != "" && w.Claiming == "" {
				w.Claiming = claiming
			}
			return
		}
		merged[name] = &Worker{Name: name, Claiming: claiming}
	}

	rows, err := s.db.Query(`SELECT c.user_name, f.name FROM claim c JOIN folder f ON f.id=c.folder_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u, fn string
		if err := rows.Scan(&u, &fn); err != nil {
			rows.Close()
			return nil, err
		}
		add(u, fn)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	krows, err := s.db.Query(`SELECT name FROM known_user WHERE last_used >= ?`, cutoffMs)
	if err != nil {
		return nil, err
	}
	for krows.Next() {
		var u string
		if err := krows.Scan(&u); err != nil {
			krows.Close()
			return nil, err
		}
		add(u, "")
	}
	krows.Close()
	if err := krows.Err(); err != nil {
		return nil, err
	}

	out := make([]Worker, 0, len(merged))
	for _, w := range merged {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// QCStatsAll 全局质量标记汇总（不限目录），给仪表盘用。Total 为有 QC 记录的文件数。
func (s *Store) QCStatsAll() (QCStat, error) {
	var st QCStat
	err := s.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN (qc.flags & ?)>0 THEN 1 ELSE 0 END),0)
		FROM file f JOIN qc ON qc.file_id = f.id
		WHERE f.present=1`,
		qc.FlagDup, qc.FlagCorrupted, qc.FlagBlank, qc.FlagLensDirt,
		qc.FlagBlur, qc.FlagExposure, qc.FlagNoise, qc.FlagShake).
		Scan(&st.Total, &st.Dup, &st.Corrupt, &st.Blank, &st.Dirt,
			&st.Blur, &st.Exposure, &st.Noise, &st.Shake)
	return st, err
}

