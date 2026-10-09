package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"medreview/qc"
)

// 验证 QC 接入 magick 后：相机 RAW / HEIC / AVIF 这类 ffmpeg 解不开的格式，
// 不再被 runImage 误判成「损坏」，而是经 ImageMagick 转成 JPEG 后正常做 QC。
func TestQCAnalyzePathMagickFormat(t *testing.T) {
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	ffmpeg := findTool("ffmpeg")
	if ffmpeg == "" {
		t.Skip("没装 ffmpeg，RunMedia 不可用")
	}

	st := newTestStore(t)
	root := st.Root()

	// 造一张 AVIF（ffmpeg 解不开、会被旧逻辑误判损坏的格式）
	avifPath := filepath.Join(root, "t.avif")
	if out, err := exec.Command(magick, "-size", "64x48", "xc:blue", "avif:"+avifPath).CombinedOutput(); err != nil {
		t.Fatalf("生成 AVIF 失败: %v\n%s", err, out)
	}
	info, _ := os.Stat(avifPath)

	m := &QCManager{store: st, magick: magick}

	f := FileItem{ID: 1, RelPath: "t.avif", Kind: kindImage, Size: info.Size(), MTime: 1}
	src := m.analyzePath(context.Background(), f)
	if src == "" {
		t.Fatal("magick 格式应当返回可分析的临时 JPEG，却返回了空（被跳过）")
	}
	defer os.Remove(src)
	abs := st.absPath("t.avif")
	if src == abs {
		t.Fatal("magick 格式应转成临时文件，不应直接返回源文件")
	}
	if !strings.HasSuffix(src, ".jpg") {
		t.Fatalf("analyzePath 产物应是 .jpg，实际 %q", src)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Fatalf("临时 JPEG 不存在: %v", serr)
	}

	// 关键：把这个临时 JPEG 交给 RunMedia，应当不再是「损坏」
	svc := qc.NewQCService(ffmpeg, findTool("ffprobe"), t.TempDir())
	res, err := svc.RunMedia(context.Background(), src, false)
	if err != nil {
		t.Fatalf("RunMedia 报错: %v", err)
	}
	if res == nil {
		t.Fatal("RunMedia 返回 nil（无 ffmpeg？）")
	}
	if res.Corrupted {
		t.Fatal("AVIF 经 magick 转码后不应被判为损坏 —— 这正是要修的假阳性")
	}
}

// magick 缺失时，magick 格式应当「跳过」而非「误判损坏」：analyzePath 返回空。
func TestQCAnalyzePathMagickSkippedWithoutMagick(t *testing.T) {
	st := newTestStore(t)
	root := st.Root()
	avifPath := filepath.Join(root, "t.avif")
	if err := os.WriteFile(avifPath, []byte("not really avif"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &QCManager{store: st, magick: ""} // 没装 magick
	f := FileItem{ID: 1, RelPath: "t.avif", Kind: kindImage, Size: 16, MTime: 1}
	if got := m.analyzePath(context.Background(), f); got != "" {
		t.Fatalf("没装 magick 时应跳过（返回空），实际返回 %q", got)
	}
}

// 普通 jpg 不应走 magick：直接返回源路径，不产生临时文件。
func TestQCAnalyzePathPlainImageUnchanged(t *testing.T) {
	st := newTestStore(t)
	m := &QCManager{store: st, magick: findTool("magick")}
	f := FileItem{ID: 1, RelPath: "a.jpg", Kind: kindImage, Size: 1, MTime: 1}
	got := m.analyzePath(context.Background(), f)
	if got != st.absPath("a.jpg") {
		t.Fatalf("普通 jpg 应直接返回源路径，实际 %q", got)
	}
}
