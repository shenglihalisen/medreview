package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// setupStdin 把标准输入换成一段固定文本，并重建共享 Scanner。
// 必须重建：包级 stdinScanner 是绑在旧 os.Stdin 上的，不换会读到上一个测试的缓冲。
func setupStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if input != "" {
			w.WriteString(input)
		}
		w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	stdinScanner = bufio.NewScanner(os.Stdin)
	t.Cleanup(func() {
		os.Stdin = old
		stdinScanner = bufio.NewScanner(os.Stdin)
		r.Close()
	})
}

func writeUsersFile(t *testing.T, path string, users map[string]string, admins []string) {
	t.Helper()
	doc := map[string]any{"users": users, "admins": admins}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 数量填 0 时，没有账号文件：什么都不做，正常退出。
func TestSetupZeroNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	t.Setenv("MEDREVIEW_ADMINS", "0")
	setupStdin(t, "")
	if code := runSetup(path, false); code != 0 {
		t.Fatalf("退出码应为 0，实际 %d", code)
	}
	if fileExists(path) {
		t.Fatal("不该凭空创建 users.json")
	}
}

// 数量填 0 + 已有账号：回答 n（或直接回车）必须保留文件。
// 这是之前最危险的行为：无条件 os.Remove，一个 0 就把团队账号全删了。
func TestSetupZeroKeepsAccountsUnlessConfirmed(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "no\n"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "users.json")
		writeUsersFile(t, path, map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv"}, []string{"alice"})
		t.Setenv("MEDREVIEW_ADMINS", "0")
		setupStdin(t, answer)
		if code := runSetup(path, false); code != 0 {
			t.Fatalf("回答 %q 时退出码应为 0，实际 %d", answer, code)
		}
		if !fileExists(path) {
			t.Fatalf("回答 %q 时 users.json 不该被删除", answer)
		}
		users, _, err := readUsersDoc(path)
		if err != nil || len(users) != 1 {
			t.Fatalf("账号应原样保留，实际 %v / %v", users, err)
		}
	}
}

// 数量填 0 + 明确回答 y：才真正删除。
func TestSetupZeroDeletesOnYes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	writeUsersFile(t, path, map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv"}, []string{"alice"})
	t.Setenv("MEDREVIEW_ADMINS", "0")
	setupStdin(t, "y\n")
	if code := runSetup(path, false); code != 0 {
		t.Fatalf("退出码应为 0，实际 %d", code)
	}
	if fileExists(path) {
		t.Fatal("明确确认后应删除 users.json")
	}
}

// 非交互（读不到回答）：默认保留，不能因为读不到 stdin 就当用户同意删除。
func TestSetupZeroNonInteractiveKeeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	writeUsersFile(t, path, map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv"}, []string{"alice"})
	t.Setenv("MEDREVIEW_ADMINS", "0")
	setupStdin(t, "")
	if code := runSetup(path, false); code != 0 {
		t.Fatalf("退出码应为 0，实际 %d", code)
	}
	if !fileExists(path) {
		t.Fatal("非交互场景必须保留原账号文件")
	}
}

// 账号文件坏掉时，0 也不能删（删了就彻底没救了），并返回非 0 提醒。
func TestSetupZeroBrokenFileRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEDREVIEW_ADMINS", "0")
	setupStdin(t, "y\n")
	if code := runSetup(path, false); code == 0 {
		t.Fatal("无法解析的文件应返回非 0")
	}
	if !fileExists(path) {
		t.Fatal("无法解析的文件也不该被删除")
	}
}

// 重新 setup 时保留已有账号（否则每次启动都会悄悄清掉仪表盘里加的人）。
func TestSetupMergesExistingAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	writeUsersFile(t, path, map[string]string{"alice": "$2a$10$abcdefghijklmnopqrstuv"}, []string{"alice"})
	t.Setenv("MEDREVIEW_ADMINS", "1")
	// 账号 bob，密码输两次（至少 8 位）
	setupStdin(t, "bob\npass12345\npass12345\n")
	if code := runSetup(path, false); code != 0 {
		t.Fatalf("退出码应为 0，实际 %d", code)
	}
	users, admins, err := readUsersDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("应保留 alice 并新增 bob，实际 %v", users)
	}
	if users["alice"] != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Fatal("alice 的哈希不该被改动")
	}
	if !admins["alice"] || !admins["bob"] {
		t.Fatalf("两个账号都应是超管，实际 %v", admins)
	}
}

// 同名账号：新密码覆盖旧的（这是"重置密码"的正常路径）。
func TestSetupOverwritesSameName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	writeUsersFile(t, path, map[string]string{"alice": "$2a$10$oldoldoldoldoldoldoldold"}, []string{"alice"})
	t.Setenv("MEDREVIEW_ADMINS", "1")
	setupStdin(t, "alice\nnewpass99\nnewpass99\n")
	if code := runSetup(path, false); code != 0 {
		t.Fatalf("退出码应为 0，实际 %d", code)
	}
	users, _, err := readUsersDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("同名应覆盖而不是叠加，实际 %v", users)
	}
	if users["alice"] == "$2a$10$oldoldoldoldoldoldoldold" {
		t.Fatal("同名账号的密码应被更新")
	}
}
