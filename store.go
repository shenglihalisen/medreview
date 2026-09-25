package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

// absPath 把相对索引路径还原成磁盘绝对路径。
func (s *Store) absPath(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return s.root
	}
	return filepath.Join(s.root, filepath.FromSlash(rel))
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
	q := `SELECT f.id, f.name, f.rel_path, f.size, f.kind, COALESCE(r.decision,0), COALESCE(r.reviewer,''), f.mtime
		FROM file f LEFT JOIN review r ON r.file_id = f.id
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
		if err := rows.Scan(&it.ID, &it.Name, &it.RelPath, &it.Size, &it.Kind, &it.Decision, &it.Reviewer, &it.MTime); err != nil {
			return nil, err
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

func (s *Store) ClaimFolder(folderID int64, user string) error {
	_, err := s.db.Exec(`INSERT INTO claim(folder_id, user_name, claimed_at) VALUES(?,?,?)
		ON CONFLICT(folder_id) DO UPDATE SET user_name=excluded.user_name, claimed_at=excluded.claimed_at`,
		folderID, user, time.Now().Unix())
	return err
}

func (s *Store) ReleaseFolder(folderID int64, user string) error {
	_, err := s.db.Exec(`DELETE FROM claim WHERE folder_id=? AND (user_name=? OR ?='')`, folderID, user, user)
	return err
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
