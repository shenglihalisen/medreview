//go:build linux || darwin

package main

// 内嵌工具释放器（Linux / macOS 共用）：把 embed.FS 里的工具写到用户缓存目录，
// 返回目录路径；失败返回空串，findTool 会继续走 PATH 兜底。
//
// 与 Windows 版同策略：只按大小比对（内置版本更新后大小会变，自动重释）；
// 先写 .part 再 rename，避免半截文件被并发实例当成品。
// Unix 平台额外一步：内嵌文件没有执行位，释放后必须 chmod 0755。

import (
	"embed"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// extractEmbedded 把 srcDir 下的所有文件释放到用户缓存目录。
func extractEmbedded(fsys embed.FS, srcDir string) string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return ""
	}
	dst := filepath.Join(cache, "medreview", filepath.Base(srcDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return ""
	}
	entries, err := fs.ReadDir(fsys, srcDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		src, err := fsys.Open(path.Join(srcDir, name))
		if err != nil {
			continue
		}
		info, err := src.Stat()
		if err != nil {
			src.Close()
			continue
		}
		target := filepath.Join(dst, name)
		if st, err := os.Stat(target); err == nil && st.Size() == info.Size() {
			src.Close()
			continue
		}
		tmp := target + ".part"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			continue
		}
		_, cpErr := io.Copy(out, src)
		closeErr := out.Close()
		src.Close()
		if cpErr != nil || closeErr != nil {
			os.Remove(tmp)
			continue
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			continue
		}
		_ = os.Chmod(target, 0o755)
	}
	return dst
}
