package main

import (
	"context"
	"testing"
)

// 这批测试锁住「格式注册表」这个核心不变量：
// 新格式加进来了，但分类/解码器路由不能错，否则会出现
// 「相机 RAW 被当成视频去抽帧」「HEIC 被当成普通 JPEG 预览失败回退」之类的问题。

func TestFormatRegistryTier1Image(t *testing.T) {
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".tif", ".tiff", ".ico", ".dds", ".tga", ".ppm", ".pgm", ".pbm", ".sgi", ".pcx", ".xbm", ".xpm", ".sun", ".dpx", ".exr"} {
		fd, ok := formatOf(ext)
		if !ok {
			t.Errorf("%s 没在注册表里", ext)
			continue
		}
		if fd.Kind != kindImage {
			t.Errorf("%s 应该是图片，实际 kind=%d", ext, fd.Kind)
		}
		if fd.Decoder != decFFmpeg {
			t.Errorf("%s 应该走 ffmpeg，实际 decoder=%q", ext, fd.Decoder)
		}
		if !fd.Previewable {
			t.Errorf("%s 应该可预览", ext)
		}
	}
}

func TestFormatRegistryTier1Video(t *testing.T) {
	for _, ext := range []string{".mp4", ".mov", ".avi", ".mkv", ".webm", ".m4v", ".mpg", ".mpeg", ".ts", ".flv", ".mts", ".m2ts", ".mxf", ".vob", ".wmv", ".asf", ".3gp", ".ogv", ".f4v", ".m2v"} {
		fd, ok := formatOf(ext)
		if !ok {
			t.Errorf("%s 没在注册表里", ext)
			continue
		}
		if fd.Kind != kindVideo {
			t.Errorf("%s 应该是视频，实际 kind=%d", ext, fd.Kind)
		}
		if fd.Decoder != decFFmpeg {
			t.Errorf("%s 应该走 ffmpeg，实际 decoder=%q", ext, fd.Decoder)
		}
	}
}

func TestFormatRegistryTier2Magick(t *testing.T) {
	for _, ext := range []string{".cr2", ".cr3", ".nef", ".arw", ".dng", ".raf", ".rw2", ".orf", ".srw", ".pef", ".mrw", ".erf", ".sr2", ".kdc", ".dcr", ".rwz", ".heic", ".heif", ".avif", ".jxl", ".psd"} {
		fd, ok := formatOf(ext)
		if !ok {
			t.Errorf("%s 没在注册表里", ext)
			continue
		}
		if fd.Kind != kindImage {
			t.Errorf("%s 应该是图片，实际 kind=%d", ext, fd.Kind)
		}
		if fd.Decoder != decMagick {
			t.Errorf("%s 应该走 ImageMagick，实际 decoder=%q", ext, fd.Decoder)
		}
		if !fd.Previewable {
			t.Errorf("%s 应该可预览", ext)
		}
	}
}

func TestFormatRegistryUnknownSkipped(t *testing.T) {
	for _, ext := range []string{".txt", ".doc", ".exe", ".zip", ".json"} {
		if _, ok := formatOf(ext); ok {
			t.Errorf("%s 不该被当成媒体格式", ext)
		}
	}
}

// 没装 ImageMagick 时，RAW/HEIC 这类格式必须优雅降级：返回原文件（orig=true），
// 而不是去跑一个不存在的 magick 进程、或者 panic。这是「免依赖分发」的底线 ——
// 没凑齐工具时至少别把审阅页搞挂。
func TestPreviewMagickFormatFallsBackWithoutMagick(t *testing.T) {
	st := newTestStore(t)
	s := &ImageService{
		store:     st,
		dir:       t.TempDir(),
		shortSide: 1600,
		hub:       NewHub(),
		ffmpeg:    "", // 故意置空，逼出「纯 magick 格式、magick 也缺失」的最坏路径
		magick:    "",
	}
	f := &FileItem{ID: 99, RelPath: "IMG_0001.CR3", Size: 1234, MTime: 1}
	// 源文件并不存在，但降级发生在读文件之前 —— 如果代码先去 stat 源文件就得创建它。
	p, orig, err := s.preview(context.Background(), f, false)
	if err != nil {
		t.Fatalf("预览不应报错: %v", err)
	}
	if !orig {
		t.Fatal("没装 magick 时应返回原文件（orig=true）")
	}
	if p == "" {
		t.Fatal("降级后路径不应为空")
	}
}

// 装了（指向一个不存在的二进制）时也应当降级到原文件，而不是 panic。
func TestPreviewMagickMissingBinaryDegrades(t *testing.T) {
	st := newTestStore(t)
	s := &ImageService{
		store:     st,
		dir:       t.TempDir(),
		shortSide: 1600,
		hub:       NewHub(),
		ffmpeg:    "",
		magick:    "/no/such/magick.exe",
	}
	f := &FileItem{ID: 1, RelPath: "x.heic", Size: 1, MTime: 1}
	p, orig, err := s.preview(context.Background(), f, false)
	if err != nil {
		t.Fatalf("预览不应报错: %v", err)
	}
	if !orig {
		t.Fatal("magick 二进制缺失时应降级到原文件")
	}
	if p == "" {
		t.Fatal("降级后路径不应为空")
	}
}
