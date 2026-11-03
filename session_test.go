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
