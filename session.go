package main

// 「已完成全部」引子（2026-11-03）
//
// 需求：网页版目录旁有一个"已完成全部"勾选框。
//   · 勾上  → 留一个引子文件；退出时清理用户密码与审阅数据
//   · 没勾  → 不留引子；退出什么都不清
//   · 强杀等没走退出流程的场景：下次启动先读引子，有就清理、没有就照常
//
// 引子里**不允许有任何敏感信息**（用户名、密码、路径、哈希都不行），
// 所以内容就是一个固定字符串 "done\n"。它只是一个"做过决定"的记号，
// 就算被别人看到，也只能知道"这次跑完清过数据"，别的什么都推不出来。

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

const sessionDoneMarker = "medreview.done"
const sessionDoneContent = "done\n"

// markerPath 引子固定放在数据库同目录（同一个"这次会话"的家当都在那）。
func markerPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), sessionDoneMarker)
}

// setSessionDone 写/删引子。
func setSessionDone(dbPath string, done bool) error {
	p := markerPath(dbPath)
	if !done {
		return os.Remove(p)
	}
	return os.WriteFile(p, []byte(sessionDoneContent), 0o644)
}

// sessionDone 读引子：文件存在 = 上次/本次已选择"完成后清理"。
func sessionDone(dbPath string) bool {
	b, err := os.ReadFile(markerPath(dbPath))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == strings.TrimSpace(sessionDoneContent)
}

// cleanupCompletedSession 清理"已完成全部"会话：
//   · 用户密码（users.json）
//   · 审阅数据（数据库 + 自动导出的 CSV）
//   · 引子自身
//
// 只删文件，不删目录；删掉的目录下次扫描会重建。缓存、日志保留（便于排错）。
func cleanupCompletedSession(cfg appConfig, authFilePath string) {
	var victims []string

	// 密码：优先用本次会话实际使用的那份
	if authFilePath != "" {
		victims = append(victims, authFilePath)
	}
	if def := defaultUsersPath(cfg.dbPath); def != authFilePath {
		victims = append(victims, def)
	}

	// 审阅数据
	victims = append(victims,
		cfg.dbPath,
		cfg.dbPath+"-wal",
		cfg.dbPath+"-shm",
		reviewCSVMakePath(cfg.dbPath, cfg.csvPath),
		markerPath(cfg.dbPath),
	)

	for _, p := range victims {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err == nil {
			log.Printf("已清理（完成后）：%s", p)
		}
	}
}

// maybeCleanupOnStart 启动时兜底：上次如果被强杀没走到退出清理，
// 这里读引子把没清的补清掉。返回 true 表示执行了清理。
func maybeCleanupOnStart(cfg appConfig) bool {
	if !sessionDone(cfg.dbPath) {
		return false
	}
	log.Println("检测到上次会话的「已完成」引子，清理密码与审阅数据后全新启动")
	cleanupCompletedSession(cfg, "")
	return true
}
