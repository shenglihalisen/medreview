package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 清单只留一份 + 退出清理的行为约束（用户 2026-10-10 明确要求）。
//
// 头号风险是**误删用户的东西**：清理逻辑跑在程序工作目录里，
// 而用户很可能把素材目录、导出目录都放在同一层。所以必须钉死：
//只删我们自己命名的那些文件，其他一律不碰。

// 切到临时目录当"程序工作目录"，避免测试去动真实项目目录。
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func writeFileIn(t *testing.T, dir, name string, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 连续打包多次，程序目录里应该始终只有一份清单（且是最新那份内容）。
func TestWriteManifestKeepsOnlyOne(t *testing.T) {
	dir := chdirTemp(t)

	writeManifest([]byte("第一份"))
	writeManifest([]byte("第二份"))
	writeManifest([]byte("第三份"))

	csvs := listCSVs(t, dir)
	if len(csvs) != 1 {
		names := make([]string, 0, len(csvs))
		for _, c := range csvs {
			names = append(names, filepath.Base(c))
		}
		t.Fatalf("打包 3 次后应只剩 1 份清单，实际 %d 份: %v", len(csvs), names)
	}
	if filepath.Base(csvs[0]) != manifestFileName {
		t.Fatalf("清单文件名应固定为 %q，实际 %q", manifestFileName, filepath.Base(csvs[0]))
	}
	// 内容必须是最新那次
	b, _ := os.ReadFile(csvs[0])
	if string(b) != "第三份" {
		t.Fatalf("留下的应是最新那份内容，实际 %q", string(b))
	}
}

// 旧版本留下的带时间戳清单要被清掉（否则升级用户会一堆积压）。
func TestWriteManifestRemovesLegacyTimestamped(t *testing.T) {
	dir := chdirTemp(t)
	writeFileIn(t, dir, "打包清单_20260101-120000.csv", "旧的1")
	writeFileIn(t, dir, "打包清单_20260102-150405.csv", "旧的2")

	writeManifest([]byte("新的"))

	csvs := listCSVs(t, dir)
	if len(csvs) != 1 || filepath.Base(csvs[0]) != manifestFileName {
		names := []string{}
		for _, c := range csvs {
			names = append(names, filepath.Base(c))
		}
		t.Fatalf("带时间戳的旧清单应被清掉并只留一份新清单，实际: %v", names)
	}
}

// **绝对不能删用户的文件**：素材目录里的、导出到别处的 CSV、其他命名的东西。
func TestCleanupNeverTouchesUserFiles(t *testing.T) {
	dir := chdirTemp(t)

	// 用户自己的各种文件（名字里恰好有 csv 但不是我们生成的）
	mine1 := writeFileIn(t, dir, "我的导出清单.csv", "用户的")
	mine2 := writeFileIn(t, dir, "report_2026.csv", "用户的")
	mine3 := writeFileIn(t, dir, "导出清单.csv", "用户导出目录里的同名文件")
	mine4 := writeFileIn(t, dir, "照片.csv", "用户的")
	mine5 := writeFileIn(t, dir, "readme.txt", "用户的")

	// 一个子目录，里面放我们命名的东西 —— 也不能递归进去删
	sub := filepath.Join(dir, "导出目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inSub := writeFileIn(t, sub, "打包清单.csv", "子目录里的")

	writeManifest([]byte("程序自己的"))

	for _, p := range []string{mine1, mine2, mine3, mine4, mine5, inSub} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("用户/子目录的文件被误删了: %s", filepath.Base(p))
		}
	}
}

// 退出清理：keep 指定的保留，其余删掉。
func TestCleanupOnExitKeepsSpecified(t *testing.T) {
	dir := chdirTemp(t)

	keep := writeManifest([]byte("最终那份"))
	if keep == "" {
		t.Fatal("writeManifest 没返回路径")
	}
	// 制造几个旧版残留
	writeFileIn(t, dir, "打包清单_20260101-120000.csv", "旧的")

	cleanupManifestsOnExit(keep)

	// keep 必须在
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("退出清理把要保留的那份删了: %v", err)
	}
	// 旧的必须没了
	if _, err := os.Stat(filepath.Join(dir, "打包清单_20260101-120000.csv")); err == nil {
		t.Fatal("退出清理没删掉旧的带时间戳清单")
	}
	// 用户文件必须还在
	user := writeFileIn(t, dir, "我的.csv", "用户的")
	cleanupManifestsOnExit(keep)
	if _, err := os.Stat(user); err != nil {
		t.Fatal("退出清理误删了用户的 CSV")
	}
}

// 目录里没有清单时不报错（首次启动、或用户手动删光）。
func TestCleanupOnEmptyDirNoPanic(t *testing.T) {
	chdirTemp(t)
	cleanupManifestsOnExit("") // 不应 panic
	cleanupManifestFiles(".")
}

// listCSVs 列出目录第一层的 csv（不递归）。
func listCSVs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".csv") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}