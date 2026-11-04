package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 引子只允许写固定内容，绝不能带敏感信息。
func TestSessionDoneMarker(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "medreview.db")

	if sessionDone(db) {
		t.Fatal("没有引子时应为 false")
	}
	if err := setSessionDone(db, true); err != nil {
		t.Fatal(err)
	}
	if !sessionDone(db) {
		t.Fatal("写入后应为 true")
	}
	b, err := os.ReadFile(markerPath(db))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "done" {
		t.Fatalf("引子内容只能是固定字符串，实际 %q", b)
	}
	if err := setSessionDone(db, false); err != nil {
		t.Fatal(err)
	}
	if sessionDone(db) {
		t.Fatal("取消勾选后应为 false")
	}
}

// 勾选后的清理：密码、库、CSV、引子都删；缓存/日志不动。
func TestCleanupCompletedSession(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "medreview.db")
	users := filepath.Join(dir, "users.json")
	csv := reviewCSVMakePath(db, "")
	for _, p := range []string{db, users, csv} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logp := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setSessionDone(db, true); err != nil {
		t.Fatal(err)
	}

	cleanupCompletedSession(appConfig{dbPath: db}, users)

	for _, p := range []string{db, users, csv, markerPath(db)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应被清理", p)
		}
	}
	if _, err := os.Stat(logp); err != nil {
		t.Fatal("日志不应被清理")
	}
}

// 没勾选时启动不清理。
func TestMaybeCleanupOnStartKeepsData(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "medreview.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if maybeCleanupOnStart(appConfig{dbPath: db}) {
		t.Fatal("没有引子不应清理")
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("库文件应保留")
	}
}

// 启动兜底必须清理**本次实际使用的**密码文件，而不是只清数据库旁边那份。
// 自定义 -auth-file（以及 fnOS 包里密码在 /etc、库在 /var 的部署）就靠这个。
func TestMaybeCleanupOnStartCustomAuthFile(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "state")
	etcDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dbDir, "medreview.db")
	users := filepath.Join(etcDir, "users.json")
	for _, p := range []string{db, users} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := setSessionDone(db, true); err != nil {
		t.Fatal(err)
	}

	if !maybeCleanupOnStart(appConfig{dbPath: db, authFile: users}) {
		t.Fatal("有引子时应清理")
	}
	if _, err := os.Stat(users); !os.IsNotExist(err) {
		t.Fatal("自定义路径的 users.json 应被清理")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("库应被清理")
	}
	if _, err := os.Stat(markerPath(db)); !os.IsNotExist(err) {
		t.Fatal("引子应被清理")
	}
}

// 显式 -csv 指向的文件不属于本次会话的临时家当，清理时必须留着。
func TestCleanupCompletedSessionKeepsExplicitCSV(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "medreview.db")
	explicit := filepath.Join(dir, "交付", "清单.csv")
	if err := os.MkdirAll(filepath.Dir(explicit), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{db, explicit} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := setSessionDone(db, true); err != nil {
		t.Fatal(err)
	}

	cleanupCompletedSession(appConfig{dbPath: db, csvPath: explicit}, "")

	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("库应被清理")
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatal("显式指定的 CSV 不应被清理")
	}
}
