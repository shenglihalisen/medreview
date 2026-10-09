package main

// 运行期产生的临时清单文件管理：同一时刻只留最新一份，退出时把旧的清掉。
//
// 背景（用户 2026-10-10 要求）：
//
//	打包（/api/zip）每跑一次就在程序目录落一份 带时间戳的
//	「打包清单_YYYYMMDD-HHMMSS.csv」。打包十次就是十份 CSV 堆在目录里，
//	用户根本不知道哪份是最新的，也不敢删（怕把有用的那份删了）。
//
//	用户要的规则：**最终只留一份 CSV**，其余在程序关闭后自动删除。
//
// 做法：
//   - 每次生成清单前，先把**程序自己生成的**旧清单删掉（按名字模式匹配，
//     只删本程序产出的，绝不碰用户导出目录里的任何文件）；
//   - 清单名固定成「打包清单.csv」（不再带时间戳），天然就只有一份；
//   - 退出时再扫一遍，确保不留残余。
//
// 安全边界（重要）：
//   - 只在**程序工作目录**（cwd）里删，且只删符合我们命名模式的文件；
//   - 用户在「导出到目录」里拷过去的照片、放在任意路径的 CSV，一律不碰；
//   - 排除掉本进程正在写的那一份，避免自己把自己删了。

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// manifestFileName 是打包清单的固定文件名。
//
// 为什么去掉时间戳：带时间戳的语义是"每次一份、互不相同"，用户要的是
// "始终一份、新的覆盖旧的"。固定名字从根本上杜绝了堆积 —— 不依赖清理逻辑，
// 也不会在清理出 bug 时残留一地 CSV。
const manifestFileName = "打包清单.csv"

// legacyManifestPrefix 是旧版本留下的带时间戳清单的命名前缀
// （打包清单_20060102-150405.csv）。它们要被清掉，但只清程序自己生成的。
const legacyManifestPrefix = "打包清单_"

// writeManifest 写清单到程序工作目录：**先清掉旧的，再写新的**，
// 保证任意时刻程序目录里最多只有一份清单。
//
// 返回实际写出的路径；失败时返回空串（调用方据此决定要不要报错）。
func writeManifest(mb []byte) string {
	dir, err := os.Getwd()
	if err != nil {
		log.Printf("取工作目录失败，写清单跳过: %v", err)
		return ""
	}
	cleanupManifestFiles(dir)

	name := filepath.Join(dir, manifestFileName)
	if err := os.WriteFile(name, mb, 0o644); err != nil {
		log.Printf("写打包清单失败: %v", err)
		return ""
	}
	log.Printf("打包清单: %s", name)
	return name
}

// cleanupManifestFiles 清掉程序工作目录里所有「程序自己生成的」清单。
//
// 只匹配两种：旧的带时间戳的（打包清单_*.csv）与当前固定名的（打包清单.csv）。
// keep 参数是要**保留**的那一份（正常是空，即全清；退出收尾时也传空）。
//
// 绝不做的事：
//   - 不递归子目录（用户导出目录可能就在旁边）；
//   - 不碰任何不匹配上述模式的文件；
//   - 不碰用户自己改名过的 CSV。
func cleanupManifestFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		// 只认我们自己的两种命名，其他人的 CSV 一律不动
		if name != manifestFileName && !strings.HasPrefix(name, legacyManifestPrefix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			log.Printf("已清理旧的清单: %s", name)
		}
	}
}

// cleanupManifestsOnExit 退出时调用：把程序目录里自己生成的清单清干净。
//
// 保留**最后一次写出的那一份**（keepPath 为空时全清）。这样用户关掉程序后
// 目录里既没有堆积，也不会把他刚打包出来的最终清单弄丢。
func cleanupManifestsOnExit(keepPath string) {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := ""
	if keepPath != "" {
		if abs, aerr := filepath.Abs(keepPath); aerr == nil {
			keep = abs
		} else {
			keep = keepPath
		}
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		if name != manifestFileName && !strings.HasPrefix(name, legacyManifestPrefix) {
			continue
		}
		p := filepath.Join(dir, name)
		if abs, aerr := filepath.Abs(p); aerr == nil && abs == keep {
			continue // 这是要留下的那一份
		}
		if err := os.Remove(p); err == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("退出清理: 删除 %d 份旧清单（保留 %s）", n, filepath.Base(keep))
	}
}