package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ImageService 给大图生成一份低清晰度的 JPEG 预览并缓存，让审阅页加载得快。
//
// 设计与 VideoService 同构，但有两个关键差别：
//
//  1. **懒转码，不预热**。图片缩放是毫秒级~百毫秒级的（对比 HEVC 视频的一两分钟），
//     点开时现转完全来得及，没必要占用启动时间。
//  2. **产物默认跨启动保留**。一张预览几百 KB，清掉重转的等待远大于占用的磁盘；
//     只在「转码规格变了」和「换了素材根目录」时清空（PrepareCache / Purge）。
//
// 缓存键 = sha1(RelPath|Size|MTime|规格标签) —— 和视频一样，素材原地换了内容
// （scanner 已作废记录）或改了 -ires 都不会命中老产物。
//
// 出错兜底：任何转码失败都**回退给原文件**，绝不因为预览挂了把审阅页搞挂。
//
// EXIF 方向：手机竖拍的照片靠 EXIF orientation 标记显示为竖版。
// ffmpeg（9.x）转码时会自动把像素转正并抹掉 EXIF 标记（已实测：
// orientation=6 的 4000x1000 横图 → 输出 250x1000 竖图、左红右蓝变上红下蓝），
// 所以目标尺寸必须按「转正后的显示尺寸」算，否则会把竖图压扁。
// Go 标准库读不到 EXIF orientation，这里手写了一个最小的 JPEG APP1/TIFF 解析。
type ImageService struct {
	store     *Store
	dir       string // 缓存根目录（里面再分 iprev/）
	shortSide int    // 较小边封顶到这个值；0 = 一律原图（功能关闭）
	ffmpeg    string

	keyLocks sync.Map // cache key -> *sync.Mutex，同一张图的并发请求只转一次

	warmMu     sync.Mutex
	warmCancel context.CancelFunc // 换目录时取消上一轮预热
}

const imagePreviewDir = "iprev"

// NewImageService 创建图片预览服务。只建目录，不碰缓存内容 ——
// 清理统一走 PrepareCache（必须在端口占住之后调用，理由同视频那边的注释）。
func NewImageService(store *Store, cacheDir string, shortSide int) (*ImageService, error) {
	s := &ImageService{store: store, dir: cacheDir, shortSide: shortSide}
	s.ffmpeg = findTool("ffmpeg")
	if shortSide <= 0 {
		log.Println("图片预览: 已关闭（-ires 0，一律原图）")
		return s, nil
	}
	if s.ffmpeg == "" {
		log.Println("图片预览: 未找到 ffmpeg —— 大图将直接给原文件（加载慢但不影响使用）")
		return s, nil
	}
	log.Printf("图片预览: 就绪（较小边≤%d，点开时现转+缓存，原图可用「看原图」查看）", shortSide)
	return s, nil
}

func (s *ImageService) tag() string { return fmt.Sprintf("i%d", s.shortSide) }

func (s *ImageService) key(f *FileItem) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%s", f.RelPath, f.Size, f.MTime, s.tag())))
	return hex.EncodeToString(sum[:])
}

func (s *ImageService) outPath(f *FileItem) string {
	k := s.key(f)
	return filepath.Join(s.dir, imagePreviewDir, k[:2], k+".jpg")
}

func (s *ImageService) profileFile() string {
	return filepath.Join(s.dir, imagePreviewDir, ".profile")
}

// Preview 返回该图片应该给浏览器看的文件：要么转码产物，要么原文件。
// orig=true 表示直接给原文件（本来就够小 / 功能关闭 / 转码失败兜底）。
func (s *ImageService) Preview(ctx context.Context, f *FileItem) (path string, orig bool, err error) {
	return s.preview(ctx, f, true)
}

// preview 是 Preview 的实现体。logFail=false 时不打印转码失败日志（预热路径用，
// 损坏文件每进一次目录都会试一次，打日志就是刷屏；反正兜底是回退原图）。
func (s *ImageService) preview(ctx context.Context, f *FileItem, logFail bool) (path string, orig bool, err error) {
	src := s.store.absPath(f.RelPath)
	if s.shortSide <= 0 || s.ffmpeg == "" {
		return src, true, nil
	}

	dispW, dispH, known := imageDisplaySize(src)
	targetW, targetH, need := 0, 0, false
	if known {
		targetW, targetH, need = imgTargetSize(dispW, dispH, s.shortSide)
	} else {
		// Go 解不开的格式（HEIC/WEBP 等）：浏览器多半也放不了，顺手转成 JPEG，
		// 交给 ffmpeg 的 fit-in-box 过滤器自己算尺寸（只缩小不放大）。
		need = true
	}
	if !need {
		return src, true, nil
	}

	out := s.outPath(f)
	if _, serr := os.Stat(out); serr == nil {
		return out, false, nil // 缓存命中
	}

	// 同一张图并发请求时只让一个去转，其余的等它出结果再查缓存。
	k := s.key(f)
	lockIface, _ := s.keyLocks.LoadOrStore(k, &sync.Mutex{})
	lock := lockIface.(*sync.Mutex)
	lock.Lock()
	defer func() {
		lock.Unlock()
		// 用完就丢：否则每转一张图就常驻一个 mutex，长时间跑下来只增不减。
		// 丢掉不影响正确性 —— 后来的请求会新建一把锁，而它拿锁后的第一件事
		// 就是再查一次缓存（上面那条），此时产物已经在磁盘上了，直接命中。
		s.keyLocks.Delete(k)
	}()

	// 拿到锁后再查一次缓存（排队等来的请求大概率直接命中）
	if _, serr := os.Stat(out); serr == nil {
		return out, false, nil
	}

	if terr := s.transcode(ctx, src, out, targetW, targetH, known); terr != nil {
		if logFail {
			log.Printf("图片预览: %s 转码失败，回退原图: %v", f.RelPath, terr)
		}
		return src, true, nil
	}
	return out, false, nil
}

// WarmFolder 预热一个目录层（见 warm）。
func (s *ImageService) WarmFolder(folderID int64) { s.warm([]int64{folderID}) }

// WarmFolders 同时预热多个目录层（「含父目录」时用：本层 + 上一层）。
func (s *ImageService) WarmFolders(ids ...int64) { s.warm(ids) }

// warm 在后台预热若干目录层里的全部图片（进入目录 / 翻页时由 handleFiles 触发，
// 与视频的启动预热不同：图片按目录粒度、进了才转）。缓存已命中的直接跳过；
// 换目录（新一轮预热）会取消上一轮没跑完的。
func (s *ImageService) warm(folderIDs []int64) {
	if s.shortSide <= 0 || s.ffmpeg == "" {
		return
	}
	s.warmMu.Lock()
	if s.warmCancel != nil {
		s.warmCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.warmCancel = cancel
	s.warmMu.Unlock()

	var files []FileItem
	for _, fid := range folderIDs {
		fs, err := s.store.FolderImages(fid)
		if err != nil {
			continue
		}
		files = append(files, fs...)
	}
	if len(files) == 0 {
		return
	}
	var todo []FileItem
	for i := range files {
		f := files[i]
		if _, serr := os.Stat(s.outPath(&f)); serr == nil {
			continue // 缓存已就绪
		}
		todo = append(todo, f)
	}
	if len(todo) == 0 {
		return
	}
	log.Printf("图片预览: 开始预热（待转 %d 张 / 共 %d 张）", len(todo), len(files))

	var wg sync.WaitGroup
	sem := make(chan struct{}, 3) // 图片转码很快，3 并发足够又不占满 CPU
	var conv int64
	for i := range todo {
		if ctx.Err() != nil {
			break
		}
		f := todo[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if _, orig, perr := s.preview(ctx, &f, false); perr == nil && !orig {
				atomic.AddInt64(&conv, 1)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() == nil {
		log.Printf("图片预览: 预热完成（本轮转码 %d 张）", atomic.LoadInt64(&conv))
	}
}

// transcode 把 src 缩放后写成 out（JPEG q≈85）。known=true 时用精确目标尺寸；
// false 时用 fit-in-box 过滤器（配合 min() 防止小图被放大）。
func (s *ImageService) transcode(ctx context.Context, src, out string, w, h int, known bool) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	tmp := out + ".part"
	args := []string{"-y", "-v", "error", "-i", src}
	if known {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", w, h))
	} else {
		args = append(args, "-vf",
			fmt.Sprintf("scale=w='min(iw,%d)':h='min(ih,%d)':force_original_aspect_ratio=decrease", s.shortSide, s.shortSide))
	}
	args = append(args, "-frames:v", "1", "-q:v", "3", "-f", "image2", tmp)
	cmd := exec.CommandContext(ctx, s.ffmpeg, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(errBuf.String()))
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// PrepareCache 处理上一轮留下的图片预览缓存。
// 与视频不同：图片产物**默认保留复用**（文件小、重转费时间），只清 .part 残渣；
// 规格变了（-ires 改过）才整体清空。
func (s *ImageService) PrepareCache() error {
	d := filepath.Join(s.dir, imagePreviewDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	tag := s.tag()
	old := ""
	if b, err := os.ReadFile(s.profileFile()); err == nil {
		old = strings.TrimSpace(string(b))
	}
	if old == tag {
		// 规格没变：只清转码中途留下的 .part 残渣
		partFiles, _ := filepath.Glob(filepath.Join(d, "*", "*.part"))
		for _, p := range partFiles {
			os.Remove(p)
		}
		return nil
	}
	if old != "" {
		log.Printf("图片预览: 分辨率从 %s 改为 %s，清空旧缓存", old, tag)
	}
	if err := s.purgeDir(d); err != nil {
		return err
	}
	return os.WriteFile(s.profileFile(), []byte(tag), 0o644)
}

// Purge 清空全部图片预览缓存（换素材根目录时调用，与视频缓存同一条规则）。
func (s *ImageService) Purge() error {
	d := filepath.Join(s.dir, imagePreviewDir)
	if err := s.purgeDir(d); err != nil {
		return err
	}
	return os.WriteFile(s.profileFile(), []byte(s.tag()), 0o644)
}

func (s *ImageService) purgeDir(d string) error {
	entries, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		p := filepath.Join(d, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			continue // 保留 .profile
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// imageDisplaySize 返回图片「转正后」的显示尺寸。
// JPEG 会解析 EXIF orientation（5/6/7/8 表示宽高互换）；其余格式按原始尺寸。
// 解不开返回 known=false（可能是 HEIC/WEBP/损坏文件）。
func imageDisplaySize(path string) (w, h int, known bool) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer fh.Close()
	cfg, _, err := image.DecodeConfig(fh)
	if err != nil {
		return 0, 0, false
	}
	w, h = cfg.Width, cfg.Height
	if strings.EqualFold(filepath.Ext(path), ".jpg") || strings.EqualFold(filepath.Ext(path), ".jpeg") {
		if o := jpegOrientation(path); o >= 5 && o <= 8 {
			w, h = h, w
		}
	}
	return w, h, true
}

// imgTargetSize 按「较小边封顶」算预览尺寸：横屏缩高、竖屏缩宽，已经不超上限的不放大。
// 与视频那边的 transcodeProfile.targetSize 同一算法，但没有偶数对齐（JPEG 不需要）。
func imgTargetSize(srcW, srcH, shortSide int) (w, h int, need bool) {
	if srcW <= 0 || srcH <= 0 || shortSide <= 0 {
		return srcW, srcH, false
	}
	short := srcW
	if srcH < srcW {
		short = srcH
	}
	if short <= shortSide {
		return srcW, srcH, false
	}
	ratio := float64(shortSide) / float64(short)
	w = int(math.Round(float64(srcW) * ratio))
	h = int(math.Round(float64(srcH) * ratio))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h, true
}

// jpegOrientation 从 JPEG 的 EXIF（APP1 段 → TIFF IFD0）里读 orientation 标记（0x0112）。
// 只找第一个 APP1，解析不出返回 1（=正常方向）。PNG/GIF 几乎不用 EXIF 方向，不处理。
func jpegOrientation(path string) int {
	// 只读开头一小段：EXIF（APP1）一定在 JPEG 头部，没必要把几十 MB 的原图整个读进内存 ——
	// 预热是 3 并发，以前等于同时把 3 张大图全量读进来。
	b, err := readFileHead(path, 256*1024)
	if err != nil {
		return 1
	}
	// JPEG 结构：FFD8 开头，然后是一串 FFxx 段
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	pos := 2
	for pos+4 <= len(b) {
		if b[pos] != 0xFF {
			return 1
		}
		marker := b[pos+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			pos += 2 // 无长度字段的段
			continue
		}
		if pos+4 > len(b) {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(b) {
			return 1
		}
		seg := b[pos+4 : pos+2+segLen]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return exifOrientation(seg[6:])
		}
		if marker == 0xDA { // 扫描数据开始，后面不再是段结构
			return 1
		}
		pos += 2 + segLen
	}
	return 1
}

// readFileHead 只读文件开头 n 字节（文件不足 n 就全读）。
// 给 EXIF 方向解析用：它只需要 JPEG 的头，不该为读几百字节去打开几十 MB 的文件。
func readFileHead(path string, n int) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(fh, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}

// exifOrientation 在 TIFF 数据里找 IFD0 的 orientation 条目。
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder = binary.LittleEndian
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifd0 := int(bo.Uint32(tiff[4:8]))
	if ifd0 <= 0 || ifd0+2 > len(tiff) {
		return 1
	}
	n := int(bo.Uint16(tiff[ifd0 : ifd0+2]))
	for i := 0; i < n; i++ {
		off := ifd0 + 2 + i*12
		if off+12 > len(tiff) {
			return 1
		}
		if bo.Uint16(tiff[off:off+2]) == 0x0112 {
			v := bo.Uint16(tiff[off+8 : off+10])
			if v >= 1 && v <= 8 {
				return int(v)
			}
			return 1
		}
	}
	return 1
}
