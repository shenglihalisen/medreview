package qc

// 模糊 / 曝光 / 噪点 三个新检测器的阈值校准。
//
// 用法：
//   QC_DIR=<真实素材目录> QC_FFMPEG=<ffmpeg.exe> go test -run TestCalibrateNew -v ./qc/
//
// 输出两部分：
//   1) 真实素材的指标表 —— 用来确认**零误报**，并据此把阈值定在真实值之外；
//   2) 从第一张真实图合成的正例（模糊/欠曝/过曝/噪点）—— 确认检测器**真的会响**。
// 设计原则是"宁漏勿滥"：宁可放过，也不能在好素材上乱标。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCalibrateNew(t *testing.T) {
	dir := os.Getenv("QC_DIR")
	if dir == "" {
		t.Skip("未设 QC_DIR，跳过校准")
	}
	ffmpeg := os.Getenv("QC_FFMPEG")
	if ffmpeg == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpeg = p
		}
	}
	if ffmpeg == "" {
		t.Skip("未找到 ffmpeg，跳过校准")
	}
	svc := NewQCService(ffmpeg, os.Getenv("QC_FFPROBE"), "")

	var files []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".jpg", ".jpeg", ".png", ".webp":
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	if len(files) == 0 {
		t.Skip("目录里没有图片")
	}

	// 曝光指标用**总是记录**的调试字段（r.Exposure 只在命中时才非 nil，
	// 校准要看的是"没命中时离阈值多远"，所以不能用它）
	row := func(tag string, r *QCResult) {
		fmt.Fprintf(os.Stderr, "%-40s lap=%7.2f mean=%6.1f clipLo=%5.2f clipHi=%5.2f noise=%6.2f flags=%d\n",
			tag, r.LapStd, r.ExpMean, r.ExpClipLow, r.ExpClipHigh, r.NoiseFloor, r.Flags)
	}

	fmt.Fprintf(os.Stderr, "\n===== 真实素材（要求：零误报）=====\n")
	for _, p := range files {
		r, err := svc.Run(context.Background(), p)
		if err != nil || r == nil {
			continue
		}
		row(filepath.Base(p), r)
		if r.Flags&FlagBlur != 0 {
			t.Errorf("误报模糊: %s lap=%.2f（阈值 %.2f）", filepath.Base(p), r.LapStd, BlurLapStdThresh)
		}
		if r.Flags&FlagNoise != 0 {
			t.Errorf("误报噪点: %s noise=%.2f（阈值 %.2f）", filepath.Base(p), r.NoiseFloor, NoiseFloorThresh)
		}
		if r.Flags&FlagExposure != 0 {
			t.Errorf("误报曝光: %s", filepath.Base(p))
		}
	}

	fmt.Fprintf(os.Stderr, "\n===== 合成正例（要求：都能检出）=====\n")
	base := files[0]
	tmp, err := os.MkdirTemp("", "qccal")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	// want=0 表示"只记录、不强制要求命中"（用来观察刻意放过的边缘情况）。
	// 注：over_exp 会被同时标上空镜（整帧接近纯白 → 标准差也低）、
	// under_exp 会同时标上模糊（压黑后细节全无 → 拉普拉斯也小），
	// 这是合理的双重命中，不是 bug，所以断言只查"期望的位有没有置上"。
	variants := []struct {
		name string
		args []string
		want int
	}{
		{"blur_heavy", []string{"-vf", "boxblur=14:2"}, FlagBlur},
		{"blur_light", []string{"-vf", "boxblur=4:1"}, 0}, // 轻度软化，按"宁漏勿滥"刻意放过
		{"over_exp", []string{"-vf", "eq=brightness=0.6"}, FlagExposure},
		{"under_exp", []string{"-vf", "eq=brightness=-0.6"}, FlagExposure},
		{"noisy", []string{"-vf", "noise=alls=80"}, FlagNoise},
	}
	for _, v := range variants {
		out := filepath.Join(tmp, v.name+".jpg")
		cmd := exec.Command(ffmpeg, append([]string{"-y", "-v", "error", "-i", base}, append(v.args, out)...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "  生成 %s 失败: %v %s\n", v.name, err, string(b))
			continue
		}
		r, err := svc.Run(context.Background(), out)
		if err != nil || r == nil {
			continue
		}
		row(v.name, r)
		if v.want != 0 && r.Flags&v.want == 0 {
			t.Errorf("漏检: %s 期望命中 %d，实际 flags=%d", v.name, v.want, r.Flags)
		}
	}
}
