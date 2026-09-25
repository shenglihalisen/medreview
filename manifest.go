package main

// 导出清单：导出 / 打包时随文件一起给出的一份 CSV 表格。
//
// 用户要求（2026-09-25）：导出图片时不只是拿到一堆文件，还要有一张表
// 列清楚「目录位置 / 每个文件 / 是否保留」，方便对照核对。
// CSV 带 UTF-8 BOM，Excel 双击打开不乱码；两条导出链路共用：
//   - /api/copy（导出到本机目录）：清单写到目标目录里的 导出清单.csv
//   - /api/zip （打包下载）    ：清单作为 zip 里的一个条目

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// buildExportManifest 生成导出清单 CSV 内容（带 UTF-8 BOM）。
//
//	all     —— 导出范围内的**每一个**文件（含未标记的），FilesInFolders 的结果
//	base    —— 导出基准目录的 relPath（相对素材根），清单里的「目录」列相对它计算
//	status  —— 文件 id -> 导出状态（已复制/已存在跳过/失败/已打包）；不在表里的 = 未导出
//	summary —— 附加汇总行（写在表格之前），如 来源目录 / 本次复制 N 个
func buildExportManifest(all []FileItem, base string, status map[int64]string, summary [][2]string) []byte {
	// 先统计一遍，汇总行里的数字才是准的
	var keep, reject, unmarked int
	for _, f := range all {
		switch f.Decision {
		case 1:
			keep++
		case 2:
			reject++
		default:
			unmarked++
		}
	}

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM：Excel 按 UTF-8 识别
	w := csv.NewWriter(&buf)

	w.Write([]string{"medreview 导出清单"})
	for _, kv := range summary {
		w.Write(kv[:])
	}
	w.Write([]string{"生成时间", time.Now().Format("2006-01-02 15:04:05")})
	w.Write([]string{"保留", strconv.Itoa(keep)})
	w.Write([]string{"不保留", strconv.Itoa(reject)})
	w.Write([]string{"未标记", strconv.Itoa(unmarked)})
	w.Write([]string{"合计", strconv.Itoa(len(all))})
	w.Write([]string{""})
	w.Write([]string{"序号", "目录", "文件名", "是否保留", "是否已导出", "大小(MB)"})

	for i, f := range all {
		rel := strings.TrimPrefix(filepath.ToSlash(f.RelPath), "./")
		if base != "" {
			rel = strings.TrimPrefix(rel, base+"/")
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." || dir == "" {
			dir = "（根目录）"
		}
		keepTxt := "未标记"
		switch f.Decision {
		case 1:
			keepTxt = "保留"
		case 2:
			keepTxt = "不保留"
		}
		st := status[f.ID]
		if st == "" {
			st = "未导出"
		}
		w.Write([]string{
			strconv.Itoa(i + 1),
			csvSafe(dir),
			csvSafe(f.Name),
			csvSafe(keepTxt),
			csvSafe(st),
			fmt.Sprintf("%.2f", float64(f.Size)/1024/1024),
		})
	}
	w.Flush()
	return buf.Bytes()
}

// relEscapes 检查导出用的相对路径里有没有 ".." 段（防止跳出导出目标目录）。
// 正常扫描不会产生这种路径，这里是防御纵深：万一 db 被手动改过也能兜住。
func relEscapes(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// csvSafe 防 CSV/Excel 公式注入：以 = + - @ 或制表符开头的单元格会被 Excel
// 当公式执行（CVE 类经典问题）。目录/文件名来自磁盘，虽然一般是自己人放的，
// 但清单会被 Excel 打开，按审计标准统一转义（前置单引号，Excel 显示为文本）。
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
