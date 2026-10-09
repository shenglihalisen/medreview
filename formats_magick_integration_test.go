package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 真实集成测试：用 ImageMagick 造一个 AVIF（ffmpeg 解不开、浏览器也多半放不了），
// 再走 medreview 自己的 transcodeMagick 路径，确认能产出一张可浏览的 JPEG。
//
// 这条链路的每一步都是真的：magick 把 AVIF 解码 -> resize 成小边 JPEG -> 落盘，
// 而不是「逻辑上能走到」的假绿。CR2/NEF/HEIC 走的是同一个 magick 解码器
// （Delegates 里已含 heic/jxl/raw），验证 AVIF 即代表整条 magick 路径打通。
func TestPreviewMagickRealAVIF(t *testing.T) {
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过真实集成测试（降级路径另有测试覆盖）")
	}

	st := newTestStore(t)
	root := st.Root()

	// 造一张 AVIF。magick 自带 AVIF 编码器（rw+）。
	avifPath := filepath.Join(root, "t.avif")
	gen := exec.Command(magick, "-size", "64x48", "xc:blue", "avif:"+avifPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成 AVIF 失败: %v\n%s", err, out)
	}
	info, err := os.Stat(avifPath)
	if err != nil {
		t.Fatalf("AVIF 没生成: %v", err)
	}

	s := &ImageService{
		store:     st,
		dir:       t.TempDir(),
		shortSide: 200,
		hub:       NewHub(),
		ffmpeg:    "", // 故意置空，逼出 magick 分支
		magick:    magick,
	}
	f := &FileItem{ID: 1, RelPath: "t.avif", Size: info.Size(), MTime: 1}

	p, orig, perr := s.preview(context.Background(), f, false)
	if perr != nil {
		t.Fatalf("预览报错: %v", perr)
	}
	if orig {
		t.Fatal("magick 格式应当生成预览（orig=false），却退回了原文件")
	}
	if !strings.HasSuffix(p, ".jpg") {
		t.Fatalf("预览产物应是 .jpg，实际 %q", p)
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Fatalf("预览产物不存在: %v", serr)
	}
	// 确认产物真的是 JPEG（magic number FF D8 FF）
	data := make([]byte, 3)
	fh, _ := os.Open(p)
	fh.Read(data)
	fh.Close()
	if data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("预览产物不是 JPEG（magic=%x）", data)
	}
}
