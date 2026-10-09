package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 相机 RAW（CR3/NEF/ARW…）走 ImageMagick 解码的回归测试。
//
// 背景（真实故障）：预热 236 张大图时，日志刷出
//
//	IMG_3261.CR3 (ImageMagick) 转码失败，回退原图: magick: exit status 1:
//
// 注意 **stderr 是空的** —— magick 失败时往往不吐错误详情，只给一个退出码。
// 根因是相机 RAW 单文件 30~40MB、Q16 解码 6000×4000 峰值内存上百 MB，
// 临时 JPEG 又是刚落盘的新文件，杀软实时扫描 / 瞬时文件锁就会让它偶发失败。
// ffmpeg 路径早有「3 次退避重试」扛这类抖动（见 transcode 的 runAttempts），
// 但 magick 路径原先**一次失败就直接放弃**，于是整张图永久退回原文件。
//
// 本测试锁两件事：
//  1. 真实 CR3 能经 transcodeMagick 产出合法 JPEG（设 MEDREVIEW_REAL_CR3 指向真实素材时跑）；
//  2. 无论成功失败，**绝不产出 0 字节 / 非 JPEG 的坏预览**（校验 magic number）。

// TestTranscodeMagickRealCR3 用真实相机 RAW 验证解码链路。
// 用法：MEDREVIEW_REAL_CR3="D:\校庆\IMG_3260.CR3" go test -run TestTranscodeMagickRealCR3
func TestTranscodeMagickRealCR3(t *testing.T) {
	realCR3 := os.Getenv("MEDREVIEW_REAL_CR3")
	if realCR3 == "" {
		t.Skip("未设 MEDREVIEW_REAL_CR3，跳过真实 CR3 集成测试")
	}
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	if _, err := os.Stat(realCR3); err != nil {
		t.Skipf("真实素材不可访问: %v", err)
	}

	s := &ImageService{
		dir:       t.TempDir(),
		shortSide: 1600,
		hub:       NewHub(),
		magick:    magick,
	}
	out := filepath.Join(s.dir, "real_cr3_preview.jpg")

	// transcodeMagick 内部已带 3 次抗抖动重试，这里单次调用即可覆盖抖动场景。
	if err := s.transcodeMagick(context.Background(), realCR3, out); err != nil {
		t.Fatalf("真实 CR3 经 ImageMagick 转码失败: %v", err)
	}
	assertValidJPEG(t, out)
}

// TestTranscodeMagickProducesValidJPEG 自包含回归：不依赖真实素材，用 magick 造一张
// 较大尺寸的图（模拟 RAW 的解码体量），反复跑 transcodeMagick，断言每次都产出合法 JPEG。
// 这条锁的是「重试改动没把成功路径改坏」——重试最容易引入的 bug 正是
// 重试后忘记清理 / 把上一轮的半成品当结果 / moveFileWithRetry 时序写错。
func TestTranscodeMagickProducesValidJPEG(t *testing.T) {
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	st := newTestStore(t)
	root := st.Root()

	// 造一张 2400x1600 的图，体量接近一张真实 RAW 的解码负担。
	src := filepath.Join(root, "big.png")
	gen := exec.Command(magick, "-size", "2400x1600", "xc:#336699", src)
	if o, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("造测试图失败: %v\n%s", err, o)
	}

	s := &ImageService{
		store:     st,
		dir:       t.TempDir(),
		shortSide: 1600,
		hub:       NewHub(),
		magick:    magick,
	}

	// 连跑 3 次，覆盖「同一服务多次转码」的真实节奏；每次都要合法 JPEG。
	for i := 0; i < 3; i++ {
		out := filepath.Join(s.dir, "p"+string(rune('0'+i))+".jpg")
		if err := s.transcodeMagick(context.Background(), src, out); err != nil {
			t.Fatalf("第 %d 次转码失败: %v", i+1, err)
		}
		assertValidJPEG(t, out)
	}
}

// assertValidJPEG 断言产物存在、非 0 字节、且是真正的 JPEG（magic FF D8 FF）。
// 这正是原故障的漏网之处：magick 偶发「exit 0 却写出 0 字节」或写坏，
// 后续检测/前端会把它当空白或损坏，所以必须在校验里挡住。
func assertValidJPEG(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if st.Size() == 0 {
		t.Fatalf("产物是 0 字节（magick 偶发写了空文件）: %s", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("打不开产物: %v", err)
	}
	defer fh.Close()
	data := make([]byte, 3)
	if _, err := fh.Read(data); err != nil {
		t.Fatalf("读产物头失败: %v", err)
	}
	if data[0] != 0xFF || data[1] != 0xD8 || data[2] != 0xFF {
		t.Fatalf("产物不是 JPEG（magic=%x，期望 ffd8ff）: %s", data, path)
	}
}

// TestTranscodeMagickContextCancel 被取消时立刻放弃、不无限重试。
// 换素材根目录/退出时会 cancel 预热上下文，此时重试没有意义（只会拖慢退出）。
func TestTranscodeMagickContextCancel(t *testing.T) {
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	st := newTestStore(t)
	src := filepath.Join(st.Root(), "x.png")
	if o, err := exec.Command(magick, "-size", "64x48", "xc:red", src).CombinedOutput(); err != nil {
		t.Fatalf("造测试图失败: %v\n%s", err, o)
	}

	s := &ImageService{dir: t.TempDir(), shortSide: 200, hub: NewHub(), magick: magick}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	out := filepath.Join(s.dir, "cancelled.jpg")
	err := s.transcodeMagick(ctx, src, out)
	// 无论返回什么错，都不该留下半成品/0 字节文件。
	if err == nil {
		// 若恰好成功（文件极小时进程已跑完），产物也必须合法。
		assertValidJPEG(t, out)
	}
	if _, serr := os.Stat(out); serr == nil {
		st, _ := os.Stat(out)
		if st != nil && st.Size() == 0 {
			t.Fatal("取消后留下 0 字节坏文件")
		}
	}
	if strings.Contains(errString(err), "第 1 次重试") {
		t.Fatalf("上下文已取消却仍继续重试: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}