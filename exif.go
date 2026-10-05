package main

// 从 JPEG 的 EXIF 里取「设备标识」和「拍摄时间」。
//
// 用途（质量自动检测的两条领域规则，2026-09-26 定稿）：
//  1. 重复：同一秒 + 同一型号 = 连拍组，组内 pHash 允许微小差异（连拍的帧
//     hash 不会完全一致，全库"精确相同"的比对抓不到近似重复）。
//  2. 镜头污损：污点固定在镜头上，同设备的多张照片会在**画面同一位置**
//     出现同样的脏点框；单张独有的大概率是场景内容（过曝天空、灯光）。
//
// 实现说明：ffprobe 对 JPEG 的 EXIF 是不读的（实测 tags 为空），Go 标准库也没有
// EXIF 包，所以复用 image.go 里手写的 APP1/TIFF 解析思路，这里扩展到：
// IFD0 的 Make(0x010F)/Model(0x0110) + ExifIFD(0x8769) 里的 DateTimeOriginal(0x9003)。
// 只读文件头部（EXIF 一定在前面），不为几百字节打开几十 MB 的原图。

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// exifCaptureTags 返回 (device, shootKey)。
// device = Model（拿不到就用 Make，都空则返回 ""，该照片不参与连拍/污点分组）；
// shootKey = DateTimeOriginal 的秒级字符串（"2006:01:02 15:04:05"，拿不到为 ""）。
func exifCaptureTags(path string) (string, string) {
	tiff, ok := jpegExifBlob(path)
	if !ok {
		return "", ""
	}
	bo, ok := tiffByteOrder(tiff)
	if !ok {
		return "", ""
	}
	var device, shootKey string
	exifOff := -1
	eachIFDEntry(tiff, bo, int(bo.Uint32(tiff[4:8])), func(tag uint16, typ uint16, cnt uint32, valOff int) {
		switch tag {
		case 0x0110: // Model
			device = exifASCII(tiff, typ, cnt, valOff)
		case 0x010F: // Make：没有 Model 时退而求其次
			if device == "" {
				device = exifASCII(tiff, typ, cnt, valOff)
			}
		case 0x8769: // ExifIFD 指针
			exifOff = int(bo.Uint32(tiff[valOff : valOff+4]))
		}
	})
	if exifOff > 0 && exifOff+2 <= len(tiff) {
		eachIFDEntry(tiff, bo, exifOff, func(tag uint16, typ uint16, cnt uint32, valOff int) {
			if tag == 0x9003 { // DateTimeOriginal（ExifIFD）
				shootKey = exifASCII(tiff, typ, cnt, valOff)
			}
		})
	}
	device = strings.TrimSpace(device)
	shootKey = strings.TrimSpace(shootKey)
	// ⚠️ 实测这台设备的照片 EXIF 里**没有任何时间字段**（IFD0 无 0x0132、
	// ExifIFD 无 0x9003/0x9004）——所以必须拿文件名兜底：
	//   IMG_20260919_212947.jpg / VID_2026-09-19 21-29-47.mp4 → 相机标准命名
	//   r_0_1789824807743_xxx.jpg                              → epoch 毫秒
	// 文件名时间是相机写入的，跟拍摄时间是同一来源，比文件 mtime 可靠（mtime 会被拷贝改掉）。
	if shootKey == "" {
		shootKey = filenameShootKey(path)
	}
	return device, shootKey
}

// filenameShootKey 从文件名里挖拍摄时间，统一成 "2006-01-02 15:04:05"。
// 三种套路都认：IMG_20260919_212947 / Screenshot_2026-09-19-212947 / 13 位 epoch 毫秒。
func filenameShootKey(path string) string {
	name := path
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	// 1) YYYYMMDD_HHMMSS（分隔符 _ 或 -）
	for i := 0; i+15 <= len(base); i++ {
		seg := base[i : i+15]
		if isDigits(seg[:8]) && (seg[8] == '_' || seg[8] == '-') && isDigits(seg[9:]) &&
			strings.HasPrefix(seg, "20") {
			return fmt.Sprintf("%s-%s-%s %s:%s:%s", seg[0:4], seg[4:6], seg[6:8],
				seg[9:11], seg[11:13], seg[13:15])
		}
	}
	// 2) 13 位 epoch 毫秒（17~18 开头的微信/QQ 传输命名）
	for i := 0; i+13 <= len(base); i++ {
		seg := base[i : i+13]
		if !isDigits(seg) || !strings.HasPrefix(seg, "1") {
			continue
		}
		// 前后不能再是数字，避免截到更长数字串的一半
		if (i > 0 && isDigit(base[i-1])) || (i+13 < len(base) && isDigit(base[i+13])) {
			continue
		}
		ms, err := strconv.ParseInt(seg, 10, 64)
		if err != nil || ms < 1_000_000_000_000 || ms > 4_000_000_000_000 {
			continue
		}
		return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
	}
	return ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return len(s) > 0
}

// jpegExifBlob 找到 JPEG 的 APP1/Exif 段并返回其中的 TIFF 数据。
func jpegExifBlob(path string) ([]byte, bool) {
	b, err := readFileHead(path, 256*1024)
	if err != nil {
		return nil, false
	}
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, false
	}
	pos := 2
	for pos+4 <= len(b) {
		if b[pos] != 0xFF {
			return nil, false
		}
		marker := b[pos+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			pos += 2
			continue
		}
		if pos+4 > len(b) {
			return nil, false
		}
		segLen := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(b) {
			return nil, false
		}
		seg := b[pos+4 : pos+2+segLen]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return seg[6:], true
		}
		if marker == 0xDA { // 扫描数据开始，后面不再是段结构
			return nil, false
		}
		pos += 2 + segLen
	}
	return nil, false
}

func tiffByteOrder(tiff []byte) (binary.ByteOrder, bool) {
	if len(tiff) < 8 {
		return nil, false
	}
	switch string(tiff[:2]) {
	case "II":
		return binary.LittleEndian, true
	case "MM":
		return binary.BigEndian, true
	}
	return nil, false
}

// eachIFDEntry 遍历一个 IFD 的全部条目。valOff 是**值的起始下标**：
// 数据 ≤4 字节时就在条目内（off+8），否则是文件偏移，两种都统一成下标。
func eachIFDEntry(tiff []byte, bo binary.ByteOrder, ifd int, fn func(tag, typ uint16, cnt uint32, valOff int)) {
	if ifd <= 0 || ifd+2 > len(tiff) {
		return
	}
	n := int(bo.Uint16(tiff[ifd : ifd+2]))
	if n > 512 { // 防御：坏数据不至于拖死解析
		n = 512
	}
	for i := 0; i < n; i++ {
		off := ifd + 2 + i*12
		if off+12 > len(tiff) {
			return
		}
		tag := bo.Uint16(tiff[off : off+2])
		typ := bo.Uint16(tiff[off+2 : off+4])
		cnt := bo.Uint32(tiff[off+4 : off+8])
		sz := exifTypeSize(typ) * int(cnt)
		voff := off + 8
		if sz > 4 { // 值放不下条目 → 存的是偏移
			p := int(bo.Uint32(tiff[off+8 : off+12]))
			if p <= 0 || p+sz > len(tiff) {
				continue // 偏移越界：丢弃该条目，不让坏 EXIF 拖垮整体
			}
			voff = p
		}
		fn(tag, typ, cnt, voff)
	}
}

func exifTypeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7: // BYTE / ASCII / SBYTE / UNDEFINED
		return 1
	case 3, 8: // SHORT / SSHORT
		return 2
	case 4, 9, 11: // LONG / SLONG / FLOAT
		return 4
	case 5, 10, 12: // RATIONAL / SRATIONAL / DOUBLE
		return 8
	}
	return 1
}

// exifASCII 把 ASCII 条目读成字符串（去掉尾部 NUL）。
func exifASCII(tiff []byte, typ uint16, cnt uint32, valOff int) string {
	if typ != 2 || cnt == 0 || valOff+int(cnt) > len(tiff) {
		return ""
	}
	return strings.TrimRight(string(tiff[valOff:valOff+int(cnt)]), "\x00 ")
}
