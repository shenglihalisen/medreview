package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// handleZip 流式打包"标记为保留"的文件。
// 用 Store 而非 Deflate：照片和视频本身已经是压缩格式，再压一次几乎没有收益，
// 却会把 CPU 打满、把打包时间从秒级拉到分钟级。
func (h *Handler) handleZip(w http.ResponseWriter, r *http.Request) {
	if !h.canDownload(r) {
		http.Error(w, "当前页面无下载权限", http.StatusForbidden)
		return
	}
	folderID, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	if folderID <= 0 {
		http.Error(w, "缺少 folder 参数", http.StatusBadRequest)
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	scope := r.URL.Query().Get("scope")

	folder, err := h.store.FolderByID(folderID)
	if err != nil {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}

	var ids []int64
	if scope == "all" {
		ids, err = h.store.FolderDescendants(1)
	} else if recursive {
		ids, err = h.store.FolderDescendants(folderID)
	} else {
		ids = []int64{folderID}
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
		http.Error(w, "该目录下还没有标记为「保留」的文件", http.StatusNotFound)
		return
	}

	// zip 内的路径：相对于被打包的那个目录，解压出来就是干净的目录结构
	baseRel := folder.RelPath
	if scope == "all" {
		baseRel = "."
	}
	base := strings.TrimPrefix(filepath.ToSlash(baseRel), "./")
	if base == "." {
		base = ""
	}

	name := folder.Name
	if scope == "all" {
		name = "全部保留文件"
	}
	fname := fmt.Sprintf("%s_保留%d个.zip", sanitizeFileName(name), len(files))

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="medreview.zip"; filename*=UTF-8''%s`, url.QueryEscape(fname)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(w)
	written := 0
	var totalBytes int64
	clientGone := false
	for _, f := range files {
		if err := writeZipEntry(zw, h.store, f, base); err != nil {
			if isClientAbort(err) {
				// 连接已经被客户端断了，继续往里写只会一遍遍报同样的错
				clientGone = true
				break
			}
			log.Printf("打包跳过 %s: %v", f.RelPath, err)
			continue
		}
		written++
		totalBytes += f.Size
	}
	// 导出清单：一式两份 —— ① zip 里附带一份；② 运行目录旁边落一份
	// （用户要求：打包下载时，除了压缩包本身，旁边还要有一份 Excel 能打开的明细表）。
	if all, err := h.store.FilesInFolders(ids); err == nil {
		status := map[int64]string{}
		for _, f := range files {
			status[f.ID] = "已打包"
		}
		summary := [][2]string{
			{"来源目录", h.store.Root()},
			{"打包范围", fname},
			{"打包文件数", strconv.Itoa(written)},
		}
		mb := buildExportManifest(all, base, status, summary)
		if w2, e := zw.Create("导出清单.csv"); e == nil {
			w2.Write(mb)
		}
		mname := fmt.Sprintf("打包清单_%s.csv", time.Now().Format("20060102-150405"))
		if werr := os.WriteFile(mname, mb, 0o644); werr != nil {
			log.Printf("写打包清单失败: %v", werr)
		} else {
			log.Printf("打包清单: %s", mname)
		}
	}
	if err := zw.Close(); err != nil && !clientGone {
		if isClientAbort(err) {
			clientGone = true
		} else {
			log.Printf("zip 收尾失败: %v", err)
		}
	}
	if clientGone {
		// 常见原因：取消下载、提前关页面、下载完成时浏览器主动断开 —— 不是故障。
		log.Printf("zip: 客户端中断下载（%s），已停止", fname)
		return
	}
	log.Printf("打包完成: %s, %d 个文件, %.1f MB", fname, written, float64(totalBytes)/1024/1024)
}

// isClientAbort 判断错误是不是「客户端断开了连接」。
// zip/export/copy 都是边读边往连接里推，浏览器取消下载 / 关页面 /
// 下载完成时主动断开都会在这里冒出 wsasend / broken pipe 之类的错。
func isClientAbort(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"connection was aborted", // Windows WSAECONNABORTED
		"broken pipe",
		"connection reset",
		"use of closed network connection",
		"request body was lost", // http.ResponseWriter 的写入包装
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func writeZipEntry(zw *zip.Writer, store *Store, f FileItem, base string) error {
	rel := strings.TrimPrefix(filepath.ToSlash(f.RelPath), "./")
	if base != "" {
		rel = strings.TrimPrefix(rel, base+"/")
	}
	if rel == "" {
		rel = f.Name
	}
	if relEscapes(rel) {
		return fmt.Errorf("文件路径非法: %s", rel)
	}

	src := store.absPath(f.RelPath)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	fh := &zip.FileHeader{
		Name:   rel,
		Method: zip.Store,
	}
	if f.MTime > 0 {
		fh.Modified = time.Unix(f.MTime, 0)
	}
	if f.Size > 0 {
		// 预填大小（Store 模式下压缩后 = 原始大小），让 zip64 判断在写头部时就准确
		fh.UncompressedSize64 = uint64(f.Size)
		fh.CompressedSize64 = uint64(f.Size)
	}
	fw, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, in); err != nil {
		return err
	}
	return nil
}

func sanitizeFileName(s string) string {
	bad := []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r", "\t"}
	for _, b := range bad {
		s = strings.ReplaceAll(s, b, "_")
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 60 {
		s = string([]rune(s)[:60])
	}
	if s == "" {
		s = "export"
	}
	return s
}

// handleExportList 导出保留文件的路径清单，方便外部脚本（robocopy / aria2 等）使用。
//
// 必须过 canDownload：它和 zip / copy 是同一类出口（都是"把保留的东西交出去"），
// 以前唯独这里漏了校验 —— 任何人拼一个 /api/export?folder=1 就能把本机绝对路径清单
// （含用户名、完整的磁盘目录结构）拿走，还不用口令。
func (h *Handler) handleExportList(w http.ResponseWriter, r *http.Request) {
	if !h.canDownload(r) {
		http.Error(w, "当前页面无下载权限", http.StatusForbidden)
		return
	}
	folderID, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	if folderID <= 0 {
		http.Error(w, "缺少 folder 参数", http.StatusBadRequest)
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	ids := []int64{folderID}
	if recursive {
		var err error
		ids, err = h.store.FolderDescendants(folderID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	files, err := h.store.KeptFiles(ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="keep-list.txt"`)
	for _, f := range files {
		fmt.Fprintln(w, h.store.absPath(f.RelPath))
	}
}

var errNotDir = errors.New("指定路径不是目录")
