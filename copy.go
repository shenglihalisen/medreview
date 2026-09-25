package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleCopy 让后端直接在本地把「保留」的文件复制到指定目录。
// 适合大批量素材：不经过浏览器、不打包、不压缩，复制完就能用，
// 中途断了再点一次会跳过已存在的同名同大小文件（等于支持断点续来）。
//
// 只做复制，绝不做移动或删除——源文件始终只读。
func (h *Handler) handleCopy(w http.ResponseWriter, r *http.Request) {
	if !h.canDownload(r) {
		http.Error(w, "当前页面无下载权限", http.StatusForbidden)
		return
	}
	var req struct {
		FolderID  int64  `json:"folderId"`
		Recursive bool   `json:"recursive"`
		Scope     string `json:"scope"`
		Dest      string `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Dest) == "" {
		http.Error(w, "缺少目标目录", http.StatusBadRequest)
		return
	}
	if req.FolderID <= 0 {
		req.FolderID = 1
	}

	destAbs, err := filepath.Abs(req.Dest)
	if err != nil {
		http.Error(w, "目标目录路径无效", http.StatusBadRequest)
		return
	}
	rootAbs, err := filepath.Abs(h.store.Root())
	if err != nil {
		rootAbs = h.store.Root()
	}
	// 不允许导出到素材目录内部，否则会把素材复制进自己里面，下一轮扫描又翻倍
	if rootAbs != "" {
		if destAbs == rootAbs || strings.HasPrefix(destAbs, rootAbs+string(filepath.Separator)) {
			http.Error(w, "目标目录不能位于素材目录内部", http.StatusBadRequest)
			return
		}
	}
	if len(destAbs) <= 3 {
		http.Error(w, "目标目录不能是磁盘根目录", http.StatusBadRequest)
		return
	}

	folder, err := h.store.FolderByID(req.FolderID)
	if err != nil {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}
	var ids []int64
	switch {
	case req.Scope == "all":
		ids, err = h.store.FolderDescendants(1)
	case req.Recursive:
		ids, err = h.store.FolderDescendants(req.FolderID)
	default:
		ids = []int64{req.FolderID}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files, err := h.store.KeptFiles(ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(files) == 0 {
		http.Error(w, "没有标记为「保留」的文件", http.StatusNotFound)
		return
	}

	base := strings.TrimPrefix(filepath.ToSlash(folder.RelPath), "./")
	if req.Scope == "all" || base == "." {
		base = ""
	}

	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		http.Error(w, "无法创建目标目录: "+err.Error(), http.StatusBadRequest)
		return
	}

	copied, skipped, failed := 0, 0, 0
	var bytes int64
	var firstErr string
	started := time.Now()
	status := map[int64]string{} // 文件 id -> 导出状态（给导出清单用）

	for _, f := range files {
		rel := strings.TrimPrefix(filepath.ToSlash(f.RelPath), "./")
		if base != "" {
			rel = strings.TrimPrefix(rel, base+"/")
		}
		if rel == "" {
			rel = f.Name
		}
		if relEscapes(rel) {
			failed++
			status[f.ID] = "路径非法"
			if firstErr == "" {
				firstErr = "文件路径非法: " + rel
			}
			continue
		}
		dst := filepath.Join(destAbs, filepath.FromSlash(rel))
		src := h.store.absPath(f.RelPath)

		if fi, err := os.Stat(dst); err == nil && fi.Size() == f.Size && f.Size > 0 {
			skipped++
			status[f.ID] = "已存在跳过"
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			failed++
			status[f.ID] = "失败"
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		n, err := copyFile(src, dst)
		if err != nil {
			failed++
			status[f.ID] = "失败"
			if firstErr == "" {
				firstErr = err.Error()
			}
			log.Printf("复制失败 %s -> %s: %v", src, dst, err)
			continue
		}
		bytes += n
		copied++
		status[f.ID] = "已复制"
	}

	// 导出清单：范围内每一个文件（含未保留的）都列进表里 ——
	// 目录位置 / 文件名 / 是否保留 / 是否已导出，写到目标目录的 导出清单.csv。
	manifestPath := ""
	if all, err := h.store.FilesInFolders(ids); err == nil {
		summary := [][2]string{
			{"来源目录", h.store.Root()},
			{"导出目标", destAbs},
			{"本次复制", fmt.Sprintf("%d 个", copied)},
			{"已存在跳过", fmt.Sprintf("%d 个", skipped)},
			{"复制失败", fmt.Sprintf("%d 个", failed)},
		}
		mb := buildExportManifest(all, base, status, summary)
		mf := filepath.Join(destAbs, "导出清单.csv")
		if werr := os.WriteFile(mf, mb, 0o644); werr != nil {
			log.Printf("写导出清单失败: %v", werr)
		} else {
			manifestPath = mf
			log.Printf("导出清单: %s", mf)
		}
	} else {
		log.Printf("读取导出范围文件清单失败: %v", err)
	}

	log.Printf("导出完成 -> %s: 新复制 %d, 已存在跳过 %d, 失败 %d, %.1f MB, 耗时 %s",
		destAbs, copied, skipped, failed, float64(bytes)/1024/1024, time.Since(started).Round(time.Millisecond))

	writeJSON(w, map[string]any{
		"ok":       failed == 0,
		"dest":     destAbs,
		"copied":   copied,
		"skipped":  skipped,
		"failed":   failed,
		"bytes":    bytes,
		"elapsed":  time.Since(started).Round(time.Millisecond).String(),
		"error":    firstErr,
		"manifest": manifestPath,
	})
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 4*1024*1024)
	n, err := io.CopyBuffer(out, in, buf)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		// Windows 上目标已存在时 rename 会失败，这种情况直接覆盖写一次
		out2, e2 := os.Create(dst)
		if e2 != nil {
			return 0, err
		}
		in2, e3 := os.Open(src)
		if e3 != nil {
			out2.Close()
			return 0, err
		}
		n2, e4 := io.CopyBuffer(out2, in2, buf)
		out2.Close()
		in2.Close()
		if e4 != nil {
			return 0, e4
		}
		return n2, nil
	}
	return n, nil
}

// handleBrowseDest 给"导出到本机目录"用的目标目录选择器，允许新建目录。
//
// 要过 canDownload：它不只是"看"，mkdir= 会真的在磁盘上建目录，
// 没有下载权限的人不该有这个能力。它和 /api/copy 是同一条链路的前置步骤。
func (h *Handler) handleBrowseDest(w http.ResponseWriter, r *http.Request) {
	if !h.canDownload(r) {
		http.Error(w, "当前页面无下载权限", http.StatusForbidden)
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		drives := []map[string]string{}
		for _, d := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
			root := string(d) + ":\\"
			if _, err := os.Stat(root); err == nil {
				drives = append(drives, map[string]string{"name": root, "path": root})
			}
		}
		writeJSON(w, map[string]any{"path": "", "dirs": drives})
		return
	}
	mkdir := r.URL.Query().Get("mkdir")
	if mkdir != "" {
		name := filepath.Base(filepath.Clean(mkdir))
		if name == "." || name == string(filepath.Separator) {
			http.Error(w, "目录名无效", http.StatusBadRequest)
			return
		}
		target := filepath.Join(p, name)
		if err := os.MkdirAll(target, 0o755); err != nil {
			http.Error(w, "创建目录失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"path": target, "parent": p, "dirs": listDirs(target)})
		return
	}
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": listDirs(p)})
}

func listDirs(p string) []map[string]string {
	entries, err := os.ReadDir(p)
	if err != nil {
		return []map[string]string{}
	}
	out := []map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, map[string]string{"name": name, "path": filepath.Join(p, name)})
	}
	return out
}
