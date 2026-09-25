package main

import (
	"database/sql"
	"fmt"
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
	return db, nil
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
