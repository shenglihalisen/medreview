package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"medreview/qc"
)

// QC 复用预览缓存（省算力）的行为约束。
//
// 背景：相机 RAW（CR3/NEF/ARW…）只有 ImageMagick 能解，而解码 RAW 是整条链路
// 最贵的一步（36MB 文件、Q16 解码峰值上百 MB）。预览转码与 QC 原本**各解一遍
// 同一个文件**，只是输出尺寸不同 —— 纯重复劳动。现在 QC 优先拿预览缓存。
//
// ⚠️ 复用引入的头号风险是：**QC 把共享的预览缓存删掉了**。
// 缓存是跨轮次、跨功能共用的产物（用户点开那张图还要用），删了等于把「省算力」
// 变成「每次都重转」，白白多花一轮解码。所以下面这几条重点盯 owned 语义。

func newTestImageService(t *testing.T, shortSide int) (*ImageService, *Store) {
	t.Helper()
	st := newTestStore(t)
	svc, err := NewImageService(st, t.TempDir(), shortSide, NewHub())
	if err != nil {
		t.Fatalf("NewImageService 失败: %v", err)
	}
	return svc, st
}

// 造一张真实的 AVIF（ffmpeg 解不开、只有 magick 能解，正好代表 RAW 那类格式）。
func makeAVIF(t *testing.T, root string) string {
	t.Helper()
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	p := filepath.Join(root, "shot.avif")
	if out, err := exec.Command(magick, "-size", "800x600", "xc:#446688", "avif:"+p).CombinedOutput(); err != nil {
		t.Fatalf("造 AVIF 失败: %v\n%s", err, out)
	}
	return p
}

// 预览缓存已就绪时，QC 应当**零解码成本**直接复用那个文件。
func TestQCSourceReusesPreviewCache(t *testing.T) {
	svc, st := newTestImageService(t, 1600)
	avifPath := makeAVIF(t, st.Root())
	info, _ := os.Stat(avifPath)

	f := &FileItem{ID: 1, RelPath: "shot.avif", Kind: kindImage, Size: info.Size(), MTime: 1}

	// 先跑一次预览，产出缓存
	if _, _, err := svc.preview(context.Background(), f, false); err != nil {
		t.Fatalf("首次预览失败: %v", err)
	}
	cache := svc.outPath(f)
	if !fileExists(cache) {
		t.Fatalf("预览缓存没生成: %s", cache)
	}

	// QC 现在拿图，应当直接命中缓存
	got, owned, err := svc.QCSource(context.Background(), f)
	if err != nil {
		t.Fatalf("QCSource 报错: %v", err)
	}
	if got != cache {
		t.Fatalf("QC 应复用预览缓存 %q，实际返回 %q", cache, got)
	}
	if owned {
		t.Fatal("复用的预览缓存不该声明 owned —— QC 一删，用户点开那张图就得重转一次")
	}
}

// QC 通过 analyzePath 拿到复用缓存后，**绝不能把它删掉**。
// 这条是最重要的回归：owned 判错等于把「省算力」变成「每次重转」。
func TestQCRunOneKeepsSharedPreviewCache(t *testing.T) {
	svc, st := newTestImageService(t, 1600)
	magick := findTool("magick")
	if magick == "" {
		t.Skip("没装 ImageMagick，跳过")
	}
	if findTool("ffmpeg") == "" {
		t.Skip("没装 ffmpeg，跳过")
	}
	avifPath := makeAVIF(t, st.Root())
	info, _ := os.Stat(avifPath)

	f := FileItem{ID: 1, RelPath: "shot.avif", Kind: kindImage, Size: info.Size(), MTime: 1}
	if _, _, err := svc.preview(context.Background(), &f, false); err != nil {
		t.Fatalf("首次预览失败: %v", err)
	}
	cache := svc.outPath(&f)
	if !fileExists(cache) {
		t.Fatal("预览缓存没生成")
	}

	// 装配 QC 时**带上** ImageService（走复用路径）
	m := &QCManager{
		store:  st,
		magick: magick,
		images: svc,
		svc:    qc.NewQCService(findTool("ffmpeg"), findTool("ffprobe"), t.TempDir()),
	}
	m.runOne(context.Background(), f)

	// 关键断言：预览缓存必须还在
	if !fileExists(cache) {
		t.Fatal("QC 跑完后预览缓存被删了 —— 共享缓存不能由 QC 负责删除，" +
			"否则用户点开那张图要重新解码一次（复用就白做了）")
	}
}

// 预览缓存还不存在时，QCSource 应当**顺手转一份并写进缓存**，
// 这样用户之后点开该图不用再转 —— 两笔合成一笔。
func TestQCSourceWritesCacheWhenMissing(t *testing.T) {
	svc, st := newTestImageService(t, 1600)
	avifPath := makeAVIF(t, st.Root())
	info, _ := os.Stat(avifPath)

	f := &FileItem{ID: 1, RelPath: "shot.avif", Kind: kindImage, Size: info.Size(), MTime: 1}
	cache := svc.outPath(f)
	if fileExists(cache) {
		t.Fatal("前置条件：此时不该有预览缓存")
	}

	got, owned, err := svc.QCSource(context.Background(), f)
	if err != nil {
		t.Fatalf("QCSource 报错: %v", err)
	}
	if got == "" {
		t.Fatal("QCSource 返回空（AVIF 是 magick 格式，应当能转出来）")
	}
	if owned {
		t.Fatal("QCSource 写进预览缓存后不算 owned（它是共享产物）")
	}
	if !fileExists(cache) {
		t.Fatal("QCSource 应当把产物写进预览缓存，供网页预览复用")
	}
	if !fileExists(got) {
		t.Fatalf("QCSource 返回的路径不存在: %s", got)
	}
}

// 0 字节的半成品缓存不算就绪（转码中途被杀会留下这种），
// 必须重新转而不是拿它去检测。
func TestQCSourceIgnoresEmptyCache(t *testing.T) {
	svc, st := newTestImageService(t, 1600)
	avifPath := makeAVIF(t, st.Root())
	info, _ := os.Stat(avifPath)

	f := &FileItem{ID: 1, RelPath: "shot.avif", Kind: kindImage, Size: info.Size(), MTime: 1}
	cache := svc.outPath(f)
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, nil, 0o644); err != nil { // 0 字节
		t.Fatal(err)
	}

	got, _, err := svc.QCSource(context.Background(), f)
	if err != nil {
		t.Fatalf("QCSource 报错: %v", err)
	}
	if got == cache {
		t.Fatal("0 字节的半成品缓存不该被当成就绪（必须重转）")
	}
	if st, serr := os.Stat(got); serr != nil || st.Size() == 0 {
		t.Fatalf("应产出非空文件，实际 %q", got)
	}
}

// ffmpeg 能解的格式不该碰 magick，也不该产生任何 owned 文件。
func TestQCSourcePlainImageNeedsNoMagick(t *testing.T) {
	svc, st := newTestImageService(t, 1600)
	if err := os.WriteFile(filepath.Join(st.Root(), "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &FileItem{ID: 1, RelPath: "a.jpg", Kind: kindImage, Size: 1, MTime: 1}

	got, owned, err := svc.QCSource(context.Background(), f)
	if err != nil {
		t.Fatalf("QCSource 报错: %v", err)
	}
	if got != "" || owned {
		t.Fatalf("ffmpeg 能解的格式不需要预处理，应返回 (\"\", false)，实际 (%q, %v)", got, owned)
	}
}