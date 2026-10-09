package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// schema 定义全部表结构。索引围绕两个高频查询设计：
//  1. 某个目录下的文件分页列表 (folder_id, sort_key, id)
//  2. 相对路径去重 (rel_path)
const schema = `
CREATE TABLE IF NOT EXISTS folder (
	id        INTEGER PRIMARY KEY,
	parent_id INTEGER NOT NULL DEFAULT 0,
	name      TEXT NOT NULL,
	rel_path  TEXT NOT NULL UNIQUE,
	depth     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_folder_parent ON folder(parent_id);

CREATE TABLE IF NOT EXISTS file (
	id        INTEGER PRIMARY KEY,
	folder_id INTEGER NOT NULL,
	name      TEXT NOT NULL,
	sort_key  TEXT NOT NULL,
	rel_path  TEXT NOT NULL UNIQUE,
	ext       TEXT NOT NULL,
	size      INTEGER NOT NULL DEFAULT 0,
	mtime     INTEGER NOT NULL DEFAULT 0,
	kind      INTEGER NOT NULL DEFAULT 0,
	present   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_file_folder_sort ON file(folder_id, sort_key, id);
CREATE INDEX IF NOT EXISTS idx_file_folder ON file(folder_id);

CREATE TABLE IF NOT EXISTS review (
	file_id   INTEGER PRIMARY KEY,
	decision  INTEGER NOT NULL DEFAULT 0,
	reviewer  TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_review_decision ON review(decision);

CREATE TABLE IF NOT EXISTS claim (
	folder_id INTEGER PRIMARY KEY,
	user_name TEXT NOT NULL,
	claimed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

-- 质量自动检测（重复/损坏/空镜/镜头脏污）。只作辅助提示，绝不影响 review.decision。
-- 落库失败/重扫后旧结论按 size+mtime+qc_version 整体失效重算。
-- detail 是 qc.QCResult 的 JSON（含镜头脏污框坐标等），前端直接读。
CREATE TABLE IF NOT EXISTS qc (
	file_id    INTEGER PRIMARY KEY,
	flags     INTEGER NOT NULL DEFAULT 0,
	detail    TEXT NOT NULL DEFAULT '',
	dup_hash  TEXT NOT NULL DEFAULT '',
	device    TEXT NOT NULL DEFAULT '',
	shoot_key TEXT NOT NULL DEFAULT '',
	size      INTEGER NOT NULL DEFAULT 0,
	mtime     INTEGER NOT NULL DEFAULT 0,
	qc_version INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_qc_duphash ON qc(dup_hash);
CREATE INDEX IF NOT EXISTS idx_qc_folder ON qc(file_id);

-- 见过的批注名，给前端「最近用过…」下拉用。
-- 存库而不是放进程内存：关掉程序再打开也还记得（用户 2026-10-01 明确要求）。
CREATE TABLE IF NOT EXISTS known_user (
	name      TEXT PRIMARY KEY,
	last_used INTEGER NOT NULL
);
`

func openDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)",
		abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL 模式下允许多个读连接，但写操作串行化；配合 busy_timeout 足够多人并发审阅。
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	if err := ensureColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("补列失败: %w", err)
	}
	if err := normalizeClaimTimestamps(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("认领时间戳归一失败: %w", err)
	}
	return db, nil
}

// normalizeClaimTimestamps 把 claim.claimed_at 统一成**毫秒**。
//
// 背景：老版本写的是 Unix() 秒，新代码（RenewClaims / ForceReleaseStale /
// 仪表盘的僵尸认领判定）一律按 UnixMilli() 毫秒读。两边单位不一致的后果很隐蔽：
// 一个 2026 年的秒级时间戳（≈1.78e9）被当成毫秒去和"现在减 30 分钟"（≈1.78e12）比，
// 结果是**每一个老认领都判定为僵尸**，页面上一片"强制释放"。
//
// 判据：Unix 毫秒时间戳在 2001 年 9 月之前不会超过 1e12；秒级则永远小于 1e11。
// 所以用 1e12 做阈值足够宽松，不会误伤正常的秒级数据。
func normalizeClaimTimestamps(db *sql.DB) error {
	// 先看一眼有没有需要转的行；没有就别写（WAL 模式下无谓的写会触发 checkpoint）。
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM claim WHERE claimed_at > 0 AND claimed_at < ?`,
		1_000_000_000_000).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := db.Exec(
		`UPDATE claim SET claimed_at = claimed_at * 1000 WHERE claimed_at > 0 AND claimed_at < ?`,
		1_000_000_000_000); err != nil {
		return err
	}
	log.Printf("认领时间戳归一：%d 条由秒换算为毫秒", n)
	return nil
}

// wipeDB 把整库清干净：主文件 + WAL + shm 三件套一起删。
//
// 这个工具的定位是「一次一批素材、评完就走」的临时审阅台，不是长期账本 ——
// 上次的素材根目录、标记、QC 结果本就该随窗口一起消失。
// 否则第二次打开看到的是上一轮的老界面（实测上次的 root_path 会被直接复用，
// review/qc 表还留着上一轮的 132 个文件、160 条 QC）。
//
// ⚠️ 三个文件必须一起删：数据全压在 -wal 里（实测主文件 4 KB / WAL 3.8 MB），
// 只删主文件的话 SQLite 会把 WAL 重放回来，等于根本没清。
//
// ⚠️ 调用时机有讲究：Windows 上文件被句柄占着是删不掉的，
// 所以要么等 db.Close() 之后再调，要么在 openDB 之前调（此刻还没人开这个文件）。
func wipeDB(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	base := filepath.Clean(abs)
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(p); err == nil {
			log.Printf("已清除 %s", filepath.Base(p))
		}
	}
}

// ensureColumns 给**老库**补新列。CREATE TABLE IF NOT EXISTS 对已经存在的表不会加列，
// 所以每次加列都要在这里登记一条；新库走 schema 建表时列已经在了，这里的 ALTER 会因
// 列已存在而报错，所以先查 PRAGMA table_info 再决定补不补。
func ensureColumns(db *sql.DB) error {
	need := []struct{ table, name, ddl string }{
		{"qc", "device", "ALTER TABLE qc ADD COLUMN device TEXT NOT NULL DEFAULT ''"},
		{"qc", "shoot_key", "ALTER TABLE qc ADD COLUMN shoot_key TEXT NOT NULL DEFAULT ''"},
		// 仪表盘「待处理/已处理媒体」标记：图片预览生成成功、视频转码成功时置 1。
		{"file", "processed", "ALTER TABLE file ADD COLUMN processed INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range need {
		rows, err := db.Query("PRAGMA table_info(" + c.table + ")")
		if err != nil {
			return err
		}
		has := false
		for rows.Next() {
			var cid int
			var name, typ string
			var notNull, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
				rows.Close()
				return err
			}
			if name == c.name {
				has = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(c.ddl); err != nil {
				return fmt.Errorf("补列 %s.%s: %w", c.table, c.name, err)
			}
		}
	}
	return nil
}

func metaGet(db *sql.DB, k string) string {
	var v string
	_ = db.QueryRow("SELECT v FROM meta WHERE k=?", k).Scan(&v)
	return v
}

func metaSet(db *sql.DB, k, v string) error {
	_, err := db.Exec("INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v)
	return err
}
