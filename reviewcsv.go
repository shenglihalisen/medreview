package main

import (
	"database/sql"
	"encoding/csv"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// decisionText 把 0/1/2 翻成人话。0 = 没定（"标了不确定"也落在这儿）。
func decisionText(d int) string {
	switch d {
	case 1:
		return "保留"
	case 2:
		return "不保留"
	default:
		return "未定"
	}
}

// reviewCSVMakePath 算出这轮 CSV 该写哪儿：
// 默认跟数据库同目录、名字是 <库名去后缀>-review.csv（medreview.db → medreview-review.csv）。
// 显式传了 -csv 就按它来（想直接扔网盘/指定归档目录时方便）。
func reviewCSVMakePath(dbPath, want string) string {
	if strings.TrimSpace(want) != "" {
		return want
	}
	abs := dbPath
	if p, err := filepath.Abs(dbPath); err == nil {
		abs = p
	}
	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		stem = "review"
	}
	return filepath.Join(dir, stem+"-review.csv")
}

// exportReviewCSV 把这轮的审阅标记落成 CSV。
//
// 为什么退出时要存这么一份：数据库是跟着窗口一起清掉的（见 wipeDB），
// 标记只留在库里的话关一次就没了 —— 存一份 CSV 才留得住。
//
// ⚠️ 必须在 a.db.Close() 之前调：句柄一关就查不动 review 表了。
//
// 文件带 UTF-8 BOM：素材路径是中文，不带 BOM 双击进 Excel 全是乱码。
func exportReviewCSV(path string, db *sql.DB) {
	rows, err := db.Query(`select f.rel_path, r.decision, r.reviewer, r.updated_at
		from review r left join file f on f.id = r.file_id
		order by f.rel_path`)
	if err != nil {
		log.Printf("导出审阅记录失败（不影响退出）: %v", err)
		return
	}
	defer rows.Close()

	f, err := os.Create(path)
	if err != nil {
		log.Printf("导出审阅记录失败（不影响退出）: %v", err)
		return
	}
	defer f.Close()

	// BOM 必须写成转义：直接把 U+FEFF 塞进源码里，Go 会报 "invalid BOM in the middle of the file"。
	if _, err := f.WriteString("\ufeff"); err != nil {
		log.Printf("导出审阅记录失败（不影响退出）: %v", err)
		return
	}
	w := csv.NewWriter(f)
	if err := w.Write([]string{"rel_path", "decision", "decision_text", "reviewer", "updated_at"}); err != nil {
		log.Printf("导出审阅记录失败（不影响退出）: %v", err)
		return
	}
	n := 0
	for rows.Next() {
		var rel string
		var dec int
		var reviewer string
		var ts int64
		if err := rows.Scan(&rel, &dec, &reviewer, &ts); err != nil {
			log.Printf("导出审阅记录（跳过一行）: %v", err)
			continue
		}
		when := ""
		if ts > 0 {
			when = time.Unix(ts, 0).Format("2006-01-02 15:04:05")
		}
		if err := w.Write([]string{rel, strconv.Itoa(dec), decisionText(dec), reviewer, when}); err != nil {
			log.Printf("导出审阅记录失败（不影响退出）: %v", err)
			return
		}
		n++
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Printf("导出审阅记录失败（不影响退出）: %v", err)
		return
	}
	_ = rows.Err()
	log.Printf("已保存审阅记录: %s（%d 条）", path, n)
}
