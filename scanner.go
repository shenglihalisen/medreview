package main

import (
	"context"
	"database/sql"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 格式注册表：扩展名 -> 怎么处理。
//
// 早期版本只有一个扁平的 `map[ext]kind`，于是「ffmpeg 解不开的格式」一旦加进表，
// 预览就会回退成「浏览器打不开的原文件」还假装成功。现在每种格式显式声明：
//   - Kind：归图片还是视频（决定 QC / 仪表盘计数 / 前端用哪种查看器）
//   - Decoder：预览用哪个转码器。ffmpeg 解不开的（相机 RAW / HEIC / AVIF / JXL / PSD）
//     走 ImageMagick（decMagick），由它先转成 JPEG 再喂进既有管线。
//   - Previewable：能否生成缩略图。Tier1/Tier2 全部可预览；Tier3（R3D/BRAW 等
//     需要厂商 SDK 的格式）为 false，页面只显示占位图 + 下载入口。
//
// 不在表里的扩展名：扫描时直接忽略（既不入库也不展示），保持原行为。
const (
	decFFmpeg = "ffmpeg" // Go/ffmpeg 能直接解
	decMagick = "magick" // 需要 ImageMagick 转一道（相机 RAW / HEIC / AVIF / JXL / PSD）
)

type FormatDef struct {
	Kind        int
	Decoder     string
	Previewable bool
}

var mediaExt = map[string]FormatDef{
	// ---- Tier 1：ffmpeg 已能解，零新增依赖 ----
	// 图片
	".jpg":  {kindImage, decFFmpeg, true}, ".jpeg": {kindImage, decFFmpeg, true},
	".png":  {kindImage, decFFmpeg, true}, ".gif":  {kindImage, decFFmpeg, true},
	".bmp":  {kindImage, decFFmpeg, true}, ".webp": {kindImage, decFFmpeg, true},
	".tif":  {kindImage, decFFmpeg, true}, ".tiff": {kindImage, decFFmpeg, true},
	".ico":  {kindImage, decFFmpeg, true},
	".dds":  {kindImage, decFFmpeg, true}, ".tga":  {kindImage, decFFmpeg, true},
	".ppm":  {kindImage, decFFmpeg, true}, ".pgm":  {kindImage, decFFmpeg, true},
	".pbm":  {kindImage, decFFmpeg, true}, ".sgi":  {kindImage, decFFmpeg, true},
	".pcx":  {kindImage, decFFmpeg, true}, ".xbm":  {kindImage, decFFmpeg, true},
	".xpm":  {kindImage, decFFmpeg, true}, ".sun":  {kindImage, decFFmpeg, true},
	".dpx":  {kindImage, decFFmpeg, true}, ".exr":  {kindImage, decFFmpeg, true},
	// 视频
	".mp4":  {kindVideo, decFFmpeg, true}, ".mov": {kindVideo, decFFmpeg, true},
	".avi":  {kindVideo, decFFmpeg, true}, ".mkv": {kindVideo, decFFmpeg, true},
	".webm": {kindVideo, decFFmpeg, true}, ".m4v": {kindVideo, decFFmpeg, true},
	".mpg":  {kindVideo, decFFmpeg, true}, ".mpeg": {kindVideo, decFFmpeg, true},
	".ts":   {kindVideo, decFFmpeg, true}, ".flv": {kindVideo, decFFmpeg, true},
	".mts":  {kindVideo, decFFmpeg, true}, ".m2ts": {kindVideo, decFFmpeg, true}, // AVCHD
	".mxf":  {kindVideo, decFFmpeg, true}, ".vob": {kindVideo, decFFmpeg, true},
	".wmv":  {kindVideo, decFFmpeg, true}, ".asf": {kindVideo, decFFmpeg, true},
	".3gp":  {kindVideo, decFFmpeg, true}, ".ogv": {kindVideo, decFFmpeg, true},
	".f4v":  {kindVideo, decFFmpeg, true}, ".m2v": {kindVideo, decFFmpeg, true},

	// ---- Tier 2：ffmpeg 解不开，走 ImageMagick ----
	// 相机 RAW（各家厂商私有格式）
	".cr2": {kindImage, decMagick, true}, ".cr3": {kindImage, decMagick, true},
	".nef": {kindImage, decMagick, true}, ".arw": {kindImage, decMagick, true},
	".dng": {kindImage, decMagick, true}, ".raf": {kindImage, decMagick, true},
	".rw2": {kindImage, decMagick, true}, ".orf": {kindImage, decMagick, true},
	".srw": {kindImage, decMagick, true}, ".pef": {kindImage, decMagick, true},
	".mrw": {kindImage, decMagick, true}, ".erf": {kindImage, decMagick, true},
	".sr2": {kindImage, decMagick, true}, ".kdc": {kindImage, decMagick, true},
	".dcr": {kindImage, decMagick, true}, ".rwz": {kindImage, decMagick, true},
	// 新图片格式
	".heic": {kindImage, decMagick, true}, ".heif": {kindImage, decMagick, true},
	".avif": {kindImage, decMagick, true}, ".jxl":  {kindImage, decMagick, true},
	".psd":  {kindImage, decMagick, true},
}

// formatOf 查格式注册表。不在表里返回 ok=false（扫描时该文件被忽略）。
func formatOf(ext string) (FormatDef, bool) {
	if ext == "" {
		return FormatDef{}, false
	}
	fd, ok := mediaExt[strings.ToLower(ext)]
	return fd, ok
}

type ScanProgress struct {
	Running   bool   `json:"running"`
	Files     int    `json:"files"`
	Folders   int    `json:"folders"`
	Current   string `json:"current"`
	Error     string `json:"error"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt"`
	RootPath  string `json:"rootPath"`
}

type Scanner struct {
	db   *sql.DB
	hub  *Hub
	root string

	mu       sync.Mutex
	prog     ScanProgress
	cancel   context.CancelFunc
	onFinish func()
	// onChange 扫描进度每次变化时回调（装配时接 Dashboard.Mark(SecScan)）。
	onChange func()
}

func NewScanner(db *sql.DB, hub *Hub) *Scanner {
	return &Scanner{db: db, hub: hub}
}

// Progress 给 /api/status 用。RootPath 只给目录名 ——
// 扫描进度会广播给所有在线页面（含手机端），不要把本机磁盘路径带出去。
func (sc *Scanner) Progress() ScanProgress {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	p := sc.prog
	p.RootPath = rootLabel(sc.root)
	return p
}

func (sc *Scanner) set(p ScanProgress) {
	// 广播前脱敏：这条事件会推给每一个打开着页面的客户端。
	// 完整路径照样打在控制台的日志里（sc.run 里用的是本地变量 root，不受影响）。
	p.RootPath = rootLabel(p.RootPath)
	sc.mu.Lock()
	sc.prog = p
	sc.mu.Unlock()
	sc.hub.Broadcast("scan", p)
	// 扫描进度本身就是一个仪表盘区块，标脏后由增量循环推。
	// 不在这里直接推 scan 区块，是为了和其余区块走同一条路径（变了才推、且限流）。
	if sc.onChange != nil {
		sc.onChange()
	}
}

// Start 在后台 goroutine 里扫描。重复调用会先取消上一次。
func (sc *Scanner) Start(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errNotDir
	}

	sc.mu.Lock()
	if sc.cancel != nil {
		sc.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel
	sc.root = abs
	sc.mu.Unlock()

	go sc.run(ctx, abs)
	return nil
}

// fileSnap 是上一轮扫描留下的指纹。用来判断"同一个 rel_path 底下的内容有没有换过"。
// 关联键只有 rel_path（file 表唯一键），所以不做这个比对的话，
// 覆盖式替换（原地重新导出、改名后覆盖）会让旧文件的审阅结论落到新文件上 ——
// 后果是"没打过勾的文件被打进交付包"。
type fileSnap struct{ size, mtime int64 }

type fileRow struct {
	folderID int64
	name     string
	sortKey  string
	relPath  string
	ext      string
	size     int64
	mtime    int64
	kind     int
	// stale 表示这个路径下的内容相对上一轮变了，该文件的 review 必须在同一事务里作废。
	stale bool
}

func (sc *Scanner) run(ctx context.Context, root string) {
	started := time.Now()
	sc.set(ScanProgress{Running: true, Current: "准备中", StartedAt: started.Unix(), RootPath: root})

	failed := func(err error) {
		log.Printf("扫描失败: %v", err)
		sc.set(ScanProgress{Running: false, Error: err.Error(), StartedAt: started.Unix(), EndedAt: time.Now().Unix(), RootPath: root})
	}

	if _, err := sc.db.Exec(`UPDATE file SET present=0`); err != nil {
		failed(err)
		return
	}

	// 读一次上一轮的指纹快照。必须赶在任何 flush() 覆盖 size/mtime 之前读，
	// 否则下面的比对就永远是"自己跟自己比"。
	// 用 Go 侧的 map 而不是临时表：这轮扫描跨多个事务（flush 各自 Begin/Commit），
	// 而连接池有 12 条连接，SQL TEMP TABLE 会落到不同连接上"表不存在"。
	snap := make(map[string]fileSnap, 4096)
	if rows, qerr := sc.db.Query(`SELECT rel_path, size, mtime FROM file`); qerr == nil {
		for rows.Next() {
			var rp string
			var s, m int64
			if rows.Scan(&rp, &s, &m) == nil {
				snap[rp] = fileSnap{size: s, mtime: m}
			}
		}
		rows.Close()
	}

	if err := metaSet(sc.db, "root_path", root); err != nil {
		failed(err)
		return
	}

	rootName := filepath.Base(root)
	if rootName == "" || rootName == "." || rootName == string(filepath.Separator) {
		rootName = root
	}

	folderIDs := map[string]int64{}
	rootID, err := upsertFolder(sc.db, 0, rootName, ".")
	if err != nil {
		failed(err)
		return
	}
	folderIDs["."] = rootID

	batch := make([]fileRow, 0, 2000)
	fileCount, folderCount := 0, 0
	lastPush := time.Now()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := sc.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO file(folder_id, name, sort_key, rel_path, ext, size, mtime, kind, present)
			VALUES(?,?,?,?,?,?,?,?,1)
			ON CONFLICT(rel_path) DO UPDATE SET folder_id=excluded.folder_id, name=excluded.name,
				sort_key=excluded.sort_key, ext=excluded.ext, size=excluded.size,
				mtime=excluded.mtime, kind=excluded.kind, present=1`)
		if err != nil {
			tx.Rollback()
			return err
		}
		// 内容换过的路径，审阅结论一并作废。
		// 放在这个事务里（而不是扫描末尾）是有意的：末尾才做的话，
		// 一旦 walk 报错或 ctx 被取消提前 return，size/mtime 已经写进库里了，
		// 下一轮快照跟磁盘就都变成新值 —— 差异永远检测不到，脏结论永久残留。
		// 按 rel_path 删而不是按快照里的旧 id：自包含，不依赖"ON CONFLICT 不改 id"这个前提。
		delStmt, err := tx.Prepare(`DELETE FROM review WHERE file_id IN (SELECT id FROM file WHERE rel_path=?)`)
		if err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
		for _, f := range batch {
			if _, err := stmt.Exec(f.folderID, f.name, f.sortKey, f.relPath, f.ext, f.size, f.mtime, f.kind); err != nil {
				stmt.Close()
				delStmt.Close()
				tx.Rollback()
				return err
			}
			if f.stale {
				if _, err := delStmt.Exec(f.relPath); err != nil {
					stmt.Close()
					delStmt.Close()
					tx.Rollback()
					return err
				}
			}
		}
		stmt.Close()
		delStmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			return nil // 无权限 / 已删除，跳过
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "$RECYCLE.BIN" || name == "System Volume Information" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			parentRel := pathDirSlash(rel)
			pid, ok := folderIDs[parentRel]
			if !ok {
				pid = rootID
			}
			id, err := upsertFolder(sc.db, pid, name, rel)
			if err != nil {
				return nil
			}
			folderIDs[rel] = id
			folderCount++
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		fd, ok := mediaExt[ext]
		if !ok {
			return nil
		}
		kind := fd.Kind
		var size int64
		var mtime int64
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
			mtime = info.ModTime().Unix()
		}
		parentRel := pathDirSlash(rel)
		fid, ok := folderIDs[parentRel]
		if !ok {
			fid = rootID
		}
		// 内容指纹比对：size 或 mtime 任一不同就认定内容换过了。
		// 只比 size 会漏掉"等长改写"，只比 mtime 会漏掉"保留时间戳的覆盖"，用 OR 最保守。
		// （mtime 是秒精度：只 touch 不改内容会误判成换过 —— 方向是安全的，
		// 代价是重标一遍，比把错文件打进交付包好。）
		stale := false
		if old, seen := snap[rel]; seen && (old.size != size || old.mtime != mtime) {
			stale = true
		}
		batch = append(batch, fileRow{
			folderID: fid,
			name:     name,
			sortKey:  strings.ToLower(name),
			relPath:  rel,
			ext:      ext,
			size:     size,
			mtime:    mtime,
			kind:     kind,
			stale:    stale,
		})
		fileCount++

		if len(batch) >= 2000 {
			if err := flush(); err != nil {
				return err
			}
		}
		if time.Since(lastPush) > 400*time.Millisecond {
			lastPush = time.Now()
			cur := rel
			if len(cur) > 70 {
				cur = cur[len(cur)-70:]
			}
			sc.set(ScanProgress{Running: true, Files: fileCount, Folders: folderCount, Current: cur, StartedAt: started.Unix(), RootPath: root})
		}
		return nil
	})

	if err != nil && ctx.Err() == nil {
		failed(err)
		return
	}
	if ctx.Err() != nil {
		sc.set(ScanProgress{Running: false, Current: "已取消", Files: fileCount, Folders: folderCount, StartedAt: started.Unix(), EndedAt: time.Now().Unix(), RootPath: root})
		return
	}
	if err := flush(); err != nil {
		failed(err)
		return
	}

	// 清理这一轮没出现的文件和目录（含其审阅记录）
	if _, err := sc.db.Exec(`DELETE FROM review WHERE file_id IN (SELECT id FROM file WHERE present=0)`); err != nil {
		failed(err)
		return
	}
	if _, err := sc.db.Exec(`DELETE FROM file WHERE present=0`); err != nil {
		failed(err)
		return
	}
	if _, err := sc.db.Exec(`DELETE FROM claim WHERE folder_id NOT IN (SELECT id FROM folder)`); err != nil {
		failed(err)
		return
	}
	if err := pruneFolders(sc.db, folderIDs); err != nil {
		failed(err)
		return
	}
	if err := metaSet(sc.db, "scan_finished_at", time.Now().Format(time.RFC3339)); err != nil {
		failed(err)
		return
	}

	sc.set(ScanProgress{Running: false, Files: fileCount, Folders: folderCount, Current: "完成",
		StartedAt: started.Unix(), EndedAt: time.Now().Unix(), RootPath: root})
	log.Printf("扫描完成: %d 个媒体文件, %d 个目录, 耗时 %s", fileCount, folderCount, time.Since(started).Round(time.Millisecond))
	if sc.onFinish != nil {
		sc.onFinish()
	}
}

func pathDirSlash(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return "."
	}
	return rel[:i]
}

func upsertFolder(db *sql.DB, parentID int64, name, rel string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM folder WHERE rel_path=?`, rel).Scan(&id)
	if err == nil {
		if _, e := db.Exec(`UPDATE folder SET parent_id=?, name=?, depth=? WHERE id=?`,
			parentID, name, strings.Count(rel, "/")+1, id); e != nil {
			return id, e
		}
		return id, nil
	}
	res, err := db.Exec(`INSERT INTO folder(parent_id, name, rel_path, depth) VALUES(?,?,?,?)`,
		parentID, name, rel, strings.Count(rel, "/")+1)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// pruneFolders 删除本次扫描中消失的目录（只删不再存在的，保留仍存在目录已有的 id）。
func pruneFolders(db *sql.DB, seen map[string]int64) error {
	rows, err := db.Query(`SELECT id, rel_path FROM folder`)
	if err != nil {
		return err
	}
	defer rows.Close()
	stale := []int64{}
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return err
		}
		if _, ok := seen[rel]; !ok {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`DELETE FROM folder WHERE id=?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, id := range stale {
		if _, err := stmt.Exec(id); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}
