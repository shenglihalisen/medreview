package qc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type calRec struct {
	name  string
	std   float64
	flags int
	conf  float64
	boxes int
	hash  string
}
type calPair struct{ a, b string; d int }

// TestCalibrateReal 用真实素材校准阈值。默认跳过；设 QC_DIR 指向图片目录、QC_FFMPEG 指向
// ffmpeg 时运行，逐张打印统计量，并检查两两 pHash 汉明距离。结果通过 t.Log 输出。
func TestCalibrateReal(t *testing.T) {
	dir := os.Getenv("QC_DIR")
	if dir == "" {
		t.Skip("set QC_DIR to a folder of real images to run calibration")
	}
	ff := os.Getenv("QC_FFMPEG")
	svc := NewQCService(ff, os.Getenv("QC_FFPROBE"), "")

	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch filepath.Ext(p) {
		case ".jpg", ".jpeg", ".png", ".webp", ".heic", ".bmp", ".tif", ".tiff":
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("目录下没有图片文件")
	}

	recs := make([]calRec, 0, len(files))
	t.Logf("=== 逐张检测 (%d 张) ===", len(files))
	t.Logf("%-60s %8s %4s %6s %4s  %s", "文件", "std", "flags", "dirty", "框数", "pHash")
	for _, f := range files {
		r, err := svc.Run(context.Background(), f)
		if err != nil {
			t.Logf("%-60s ERROR: %v", filepath.Base(f), err)
			continue
		}
		if r == nil {
			t.Logf("%-60s (跳过: 无 ffmpeg)", filepath.Base(f))
			continue
		}
		conf, nb := 0.0, 0
		if r.LensDirt != nil {
			conf, nb = r.LensDirt.Conf, len(r.LensDirt.Boxes)
		}
		recs = append(recs, calRec{filepath.Base(f), r.GrayStd, r.Flags, conf, nb, r.DupHash})
		t.Logf("%-60s %8.2f %4d %6.3f %4d  %s", filepath.Base(f), r.GrayStd, r.Flags, conf, nb, r.DupHash)
	}

	t.Logf("\n=== pHash 两两汉明距离 (仅显示 <= %d 的对) ===", DupHammingThresh)
	var ps []calPair
	for i := 0; i < len(recs); i++ {
		for j := i + 1; j < len(recs); j++ {
			ha, hb := parseHash(recs[i].hash), parseHash(recs[j].hash)
			if d := Hamming(ha, hb); d <= DupHammingThresh {
				ps = append(ps, calPair{recs[i].name, recs[j].name, d})
			}
		}
	}
	if len(ps) == 0 {
		t.Logf("无重复对（全部 > 阈值）")
	} else {
		for _, p := range ps {
			t.Logf("  %s <-> %s 汉明=%d", p.a, p.b, p.d)
		}
	}
	t.Logf("\n统计: 空镜触发=%d 镜头脏污触发=%d 损坏=%d",
		countFlag(recs, FlagBlank), countFlag(recs, FlagLensDirt), countFlag(recs, FlagCorrupted))

	// 同时输出便于肉眼检查的诊断（写到 QC_DIR 同级的 .qc_cal.log）
	writeCalLog(dir, recs, ps, t)
}

func countFlag(recs []calRec, flag int) int {
	n := 0
	for _, x := range recs {
		if x.flags&flag != 0 {
			n++
		}
	}
	return n
}

func parseHash(s string) uint64 {
	var h uint64
	fmt.Sscanf(s, "%016x", &h)
	return h
}

func writeCalLog(dir string, recs []calRec, ps []calPair, t *testing.T) {
	lines := []string{fmt.Sprintf("逐张检测 (%d 张):", len(recs))}
	lines = append(lines, fmt.Sprintf("%-60s %8s %4s %6s %4s  %s", "文件", "std", "flags", "dirty", "框数", "pHash"))
	for _, x := range recs {
		lines = append(lines, fmt.Sprintf("%-60s %8.2f %4d %6.3f %4d  %s", x.name, x.std, x.flags, x.conf, x.boxes, x.hash))
	}
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("pHash 两两汉明距离 (<= %d):", DupHammingThresh))
	if len(ps) == 0 {
		lines = append(lines, "无重复对")
	} else {
		for _, p := range ps {
			lines = append(lines, fmt.Sprintf("  %s <-> %s 汉明=%d", p.a, p.b, p.d))
		}
	}
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("统计: 空镜=%d 镜头脏污=%d 损坏=%d",
		countFlag(recs, FlagBlank), countFlag(recs, FlagLensDirt), countFlag(recs, FlagCorrupted)))
	logPath := filepath.Join(dir, "..", ".qc_cal.log")
	if err := os.WriteFile(logPath, []byte(joinLines(lines)), 0644); err != nil {
		t.Logf("写日志失败: %v", err)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
