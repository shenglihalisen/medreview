// Package qc 对素材做质量自动检测（重复 / 损坏 / 空镜 / 镜头脏污）。
//
// 设计要点（与 photo-classifier-stable 的关系）：
//   - 只搬算法、不搬流程；结果只作辅助提示，绝不参与人工 decision。
//   - 像素统一通过已内嵌的 ffmpeg 以灰度裸数据取出（支持 heic/webp 等 Go 标准库
//     解不开的格式，且自动 EXIF 转正），之后所有判定都是 Go 原生计算。
//   - 零新依赖：不引入 OpenCV / MediaPipe；镜头脏污用纯算法局部异常检测，
//     而非依赖人脸模型的 obstruction 检测器。
//
// 本包只实现核心算法（阶段1）。落库、跨文件重复聚合、自动触发、前端呈现见主包后续阶段。
package qc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// qcVersion 在算法或阈值调整时 +1，让旧结果整体失效重算。
// 3：细节提取改为保持宽高比（模糊/噪点数值分布变了），旧结果全部重算
// 4：镜头脏污引入「同设备跨照片位置确认」、重复引入「连拍组近似匹配」，
//
//	判定语义变了，旧结果重算后由对账 pass 重标记。
const qcVersion = 5 // v5：+抖动检测（bit7）、视频全片抽样、重复像素确认

// QCVersion 导出给主包落库使用（与算法/阈值版本保持一致）。
const QCVersion = qcVersion

// 检测器位掩码
const (
	FlagDup       = 1 << iota // 重复
	FlagCorrupted             // 损坏
	FlagBlank                 // 空镜（全黑/全白/纯色）
	FlagLensDirt              // 镜头脏污 / 局部遮挡
	FlagBlur                  // 模糊（失焦 / 运动模糊）
	FlagExposure              // 曝光异常（欠曝 / 过曝）
	FlagNoise                 // 噪点（高感颗粒 / 压缩噪声）
	FlagShake                 // 抖动（手持晃动：帧间整体平移往复震荡）
)

// FlagIssueMask 所有「质量问题」位的合集。
// 注意**不含重复**：重复只是冗余，片子本身没问题，不该混进"只看问题件"里
// （否则整理重复素材时会被一堆好片淹没）。
const FlagIssueMask = FlagCorrupted | FlagBlank | FlagLensDirt | FlagBlur | FlagExposure | FlagNoise | FlagShake

// 阈值（待真实素材校准，宁漏勿滥）
const (
	GraySize   = 64   // 灰度提取边长（直方图类判定用）
	DetailSize = 1024 // 细节提取边长（模糊/噪点用，64 会把细节和噪点平均掉）
	NoiseCell  = 16   // 噪点统计的块边长（固定边长，不是固定块数）

	// 视频抽样统计（2026-10-01 用户定的规则，替代旧的"thumbnail 挑 1 帧定生死"）：
	// **全片**每秒抽 2 帧（用户后调：5→2；21:17 用户再定：不止前 30 秒，全片都要抽），
	// 同一问题在样本帧中命中超过 30% 才报。单帧的误报会被正常帧稀释掉。
	// 代价：长视频 QC 变慢（18 分钟 = 2160 帧），解码和逐帧检测都是全片的。
	VideoSampleFPS  = 2
	VideoIssueRatio = 0.3

	BlankStdThresh     = 8.0  // 全局灰度标准差低于此值判空镜
	LensDirtMinBlocks  = 3    // 至少多少异常块才算脏污
	DupHammingThresh   = 5    // pHash 汉明距离 ≤ 此值判重复（只是候选，还要过像素确认）
	DupPixelGrayThresh = 25   // 像素确认：灰度差超过此值算"显著不同"的像素
	DupPixelDiffThresh = 0.05 // 像素确认：显著不同像素占比 > 5% = 主体不同，不算重复
	// 视频判重复**只认完全相同**（阈值 0）：远景视频里主体只占画面一小块，
	// 同机位不同段的帧差只有 1~4%（实测 26 个误报），1% 都拦不住；而视频又没有
	// EXIF 时间/设备证据来套用「连拍/副本」的判定条件（用户 2026-10-01 的语义），
	// 所以只有帧序列完全相同（= 同一文件的拷贝）才判。实测：真副本 ≈0%，不同段 >0。
	DupPixelDiffThreshVideo = 0
	// 抖动检测阈值（2026-10-01 首版，合成抖动视频 + 真实三脚架素材校准）：
	ShakeRmsThresh = 1.5 // 帧间平移变化的 RMS（64×64 上的像素；1px ≈ 画面 1.5%）
	ShakeFlipRate  = 0.5 // 偏移变化的方向翻转率：往复震荡才叫抖，单向摇摄不算
	ShakeCutDiff   = 25  // 相邻帧投影匹配平均差超过此值就跳过（场景切换/大动作帧的伪偏移）

	DupTimeWindowSecs = 10 // hash 相同的两张，拍摄时间差超过此值就不是"连拍/副本"
	// （用户 2026-10-01：整体相似但时间不同、机器不同的不算重复）
	// ↑ 阈值 2026-10-01 实测于真实素材：真重复（同图传输副本）0.0%，
	//   被误判的连拍对（背景同、运动员不同）34.5%~91.4%，中间是空的，5% 两侧余量都很大。

	// 阈值 2026-09-26 二次校准于一组真实素材（14 张，细节提取已改为保持宽高比）：
	// 真实图 lap=34.3~70.9、噪声底线 0.00~1.54；重度模糊 1.78、加噪图 8.20。
	// （改宽高比前那组数是 31.6~50.8 / 1.66 / 1.58 / 9.35，阈值 15 和 5 依旧成立，故未改。）
	BlurLapStdThresh = 15.0 // 低于此值判模糊（真实最小 34.3，留 2 倍余量）
	NoiseFloorThresh = 5.0  // 高于此值判噪点（真实最大 1.54，留 3 倍余量）

	// 跨照片对账（镜头污点确认 / 连拍近似重复）
	DirtIoUThresh  = 0.5 // 两张照片的脏点框 IoU ≥ 此值 → 同一镜头污点
	DirtConfirmMin = 3   // 同设备可用照片少于这个数不做确认（没有参照物，维持单张判定）
	BurstWindowSec = 2   // 连拍时间窗：相邻两张拍摄间隔 ≤ 此值归为同一组（真连拍间隔都小于它；
	// 窗口开大了会把同一场景的不同构图误判成重复，宁漏勿滥）
	// 曝光：光看均值不行（夜景本来就暗、雪地本来就亮），必须看**死黑/死白占比**。
	// ⚠️ 过曝阈值定得高，是为了躲开**白底文档/截图**：实测 OCR 截图 mean≈240、
	// clipHi 0.63~0.75（那是正常的白纸黑字，不是过曝），而真过曝的图 clipHi=0.99。
	ExpClipLowThresh  = 0.35 // 近黑像素占比 + 整体偏暗 → 欠曝
	ExpClipHighThresh = 0.80 // 近白像素占比 + 整体偏亮 → 过曝
	ExpUnderMean      = 100.0
	ExpOverMean       = 150.0
	ExpClipLowValue   = 8   // 判定"死黑"的灰度上限
	ExpClipHighValue  = 247 // 判定"死白"的灰度下限
)

// Box 脏点框，坐标归一化到 0~1（前端按比例画到图片上）。
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// IoU 两个脏点框的交并比。镜头污点是固定在画面坐标上的，
// 同一设备两张照片的污点框会高度重叠，所以 IoU 是"真污点"的核心证据。
func (b Box) IoU(o Box) float64 {
	x0 := math.Max(b.X, o.X)
	y0 := math.Max(b.Y, o.Y)
	x1 := math.Min(b.X+b.W, o.X+o.W)
	y1 := math.Min(b.Y+b.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return 0
	}
	inter := (x1 - x0) * (y1 - y0)
	area := b.W*b.H + o.W*o.H - inter
	if area <= 0 {
		return 0
	}
	return inter / area
}

type DetectorOut struct {
	Conf float64 `json:"conf"`
}
type LensDirtOut struct {
	Conf  float64 `json:"conf"`
	Boxes []Box   `json:"boxes"`
}
type ExposureOut struct {
	Under    bool    `json:"under"`
	Over     bool    `json:"over"`
	Mean     float64 `json:"mean"`
	ClipLow  float64 `json:"clipLow"`  // 近黑像素占比
	ClipHigh float64 `json:"clipHigh"` // 近白像素占比
}

// QCResult 单次检测结果。Detail() 的 JSON 即前端读到的 f.qc。
type QCResult struct {
	Flags     int          `json:"flags"`
	DupHash   string       `json:"dupHash,omitempty"`
	Blank     *DetectorOut `json:"blank,omitempty"`
	LensDirt  *LensDirtOut `json:"lensDirt,omitempty"`
	Corrupted bool         `json:"corrupted,omitempty"`
	Blur      *DetectorOut `json:"blur,omitempty"`
	Exposure  *ExposureOut `json:"exposure,omitempty"`
	Noise     *DetectorOut `json:"noise,omitempty"`
	Shake     *ShakeOut    `json:"shake,omitempty"`

	// 以下都是校准/调试用的原始指标，不入库（json:"-"）。
	GrayStd     float64 `json:"-"` // 全局灰度标准差（空镜指标）
	LapStd      float64 `json:"-"` // 拉普拉斯标准差（模糊指标，越大越锐）
	NoiseFloor  float64 `json:"-"` // 平坦块噪声底线（噪点指标）
	ExpMean     float64 `json:"-"` // 平均亮度
	ExpClipLow  float64 `json:"-"` // 近黑像素占比
	ExpClipHigh float64 `json:"-"` // 近白像素占比
}

// ShakeOut 抖动检测结果：帧间整体平移的往复震荡强度。
type ShakeOut struct {
	Amp float64 `json:"amp"` // 帧间平移变化的 RMS（64×64 上的像素，1px ≈ 画面 1.5%）
}

// QCService 质量自动检测服务。ffmpeg 路径由调用方解析后传入，保持本包零外部依赖。
type QCService struct {
	ffmpeg  string
	ffprobe string // 取源尺寸用；为空时细节提取退回旧的正方形缩放
	dir     string // 缓存根目录（预留）

	hwMu     sync.Mutex
	hwChoice string // "cpu"/"n"=强制 CPU 软解；"cuda"/"qsv"/"d3d11va"=只用那一条；"auto"/"y"/空=三条都试后择优。bat 询问传入。
	hwMode   string // 探测成功的硬件类型：""(未探测/用 CPU)/"cuda"/"qsv"/"d3d11va"
	hwProbed bool
	hwPickedAt time.Time // 上次结论是什么时候测出来的（只用于日志）

	ms MetaStore // 可选：持久化的键/值通道，用来复用上次探测结论
}

// MetaStore 只要求一条键/值读写通道（上层拿 Store 的适配方法传进来）。
// 探测结论存在库里，下次启动直接复用，省掉每次 4 段试跑解码（本机实测 ~7 秒/轮）。
type MetaStore interface {
	MetaGet(k string) string
	MetaSet(k, v string) error
}

// NewQCService 创建检测服务。ffmpeg 为空则 Run 返回 nil（功能不可用）。
// ffprobe 只在「细节提取要保持宽高比」时需要，拿不到就退回正方形（功能照常，精度略差）。
func NewQCService(ffmpeg, ffprobe, cacheDir string) *QCService {
	if ffmpeg == "" {
		log.Println("QC: 未提供 ffmpeg 路径，质量自动检测不可用（返回空结果）")
	}
	if ffprobe == "" {
		log.Println("QC: 未提供 ffprobe，细节提取退回正方形缩放（模糊/噪点在极端宽高比上精度略差）")
	}
	return &QCService{ffmpeg: ffmpeg, ffprobe: ffprobe, dir: cacheDir}
}

// sourceSize 用 ffprobe 取源素材的像素尺寸（只读头部，很快）。
func (s *QCService) sourceSize(ctx context.Context, src string) (int, int, error) {
	if s.ffprobe == "" {
		return 0, 0, fmt.Errorf("无 ffprobe")
	}
	out, err := exec.CommandContext(ctx, s.ffprobe,
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", src).Output()
	if err != nil {
		return 0, 0, err
	}
	var w, h int
	txt := strings.TrimSpace(string(out))
	if _, e := fmt.Sscanf(txt, "%dx%d", &w, &h); e != nil {
		return 0, 0, fmt.Errorf("解析尺寸失败: %q", txt)
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("尺寸异常: %dx%d", w, h)
	}
	return w, h, nil
}

// detailSize 按源尺寸算细节提取的目标尺寸：**保持宽高比**，长边封顶 DetailSize。
// 之前是强行拉成 1024×1024 正方形，宽幅/竖幅会被非等比拉伸，
// 高频在横竖两个方向上被改得不一样，模糊/噪点的阈值就对不齐了。
func detailSize(sw, sh int) (int, int) {
	if sw <= 0 || sh <= 0 {
		return DetailSize, DetailSize
	}
	long := sw
	if sh > long {
		long = sh
	}
	if long <= DetailSize {
		return sw, sh // 本身就不大，不用放大
	}
	scale := float64(DetailSize) / float64(long)
	w := int(math.Round(float64(sw) * scale))
	h := int(math.Round(float64(sh) * scale))
	if w < 8 {
		w = 8
	}
	if h < 8 {
		h = 8
	}
	return w, h
}

// detailGray 取细节灰度（保持宽高比）。拿不到源尺寸时退回正方形。
func (s *QCService) detailGray(ctx context.Context, src string, isVideo bool) ([]byte, int, int, error) {
	w, h := DetailSize, DetailSize
	if sw, sh, err := s.sourceSize(ctx, src); err == nil {
		w, h = detailSize(sw, sh)
	}
	it, err := s.startFrameStream(ctx, src, w, h, isVideo)
	if err != nil {
		return nil, 0, 0, err
	}
	if !it.next() {
		it.close()
		return nil, 0, 0, fmt.Errorf("ffmpeg 解码失败（可能损坏）: %v", it.err)
	}
	buf := make([]byte, len(it.frame))
	copy(buf, it.frame)
	it.close()
	return buf, w, h, nil
}

// SetMetaStore 接入持久化通道（可选）。没接也能跑，只是每次启动都要重新试跑一遍探测。
// 探测结论跨启动复用意义不大？—— 有的：探测要跑 4 段 30 秒素材的完整解码
// （CUDA/核显/通用/CPU 各一次，本机约 7 秒），而硬件配置不会天天变。
func (s *QCService) SetMetaStore(ms MetaStore) {
	s.hwMu.Lock()
	s.ms = ms
	s.hwMu.Unlock()
}

// SetHWChoice 设置 QC 解码的硬件选择："y"=探测链启用显卡，"n"=强制 CPU 软解。
// 必须在探测（ProbeHWAccel）之前调用。
func (s *QCService) SetHWChoice(choice string) {
	s.hwMu.Lock()
	s.hwChoice = choice
	s.hwMu.Unlock()
}

// Available 是否具备 ffmpeg（可正常运行检测）。
func (s *QCService) Available() bool { return s.ffmpeg != "" }

// PixelDiffRatio 两张素材的"显著差异像素占比"（64×64 灰度、逐像素差 > DupPixelGrayThresh）。
// 各抽全片帧（fps=2）配对算差异取平均：**只比第一帧不够**——同机位的
// 视频首帧可能都是人物站定（几乎一样），动作差异在后面的帧里才显出来。
// 给重复判定做像素确认：pHash 只看低频，背景相同的连拍（主体人物不同）
// hash 会一样/极近 —— hash 相似只是候选，像素说了算。
// ⚠️ isVideo 必须如实传：fps=5 滤镜对单帧图片的输入会输出 0 帧（实测 dup1.jpg
// 0 字节），图片必须走单帧路径。
func (s *QCService) PixelDiffRatio(ctx context.Context, srcA, srcB string, isVideo bool) (float64, error) {
	start := func(src string) (*frameIter, error) {
		it, err := s.startFrameStream(ctx, src, GraySize, GraySize, isVideo)
		if err != nil {
			return nil, err
		}
		return it, nil
	}
	ia, err := start(srcA)
	if err != nil {
		return 0, err
	}
	ib, err := start(srcB)
	if err != nil {
		ia.close()
		return 0, err
	}
	sum, n := 0.0, 0
	for {
		if !ia.next() || !ib.next() {
			break
		}
		sig := 0
		for i := range ia.frame {
			d := int(ia.frame[i]) - int(ib.frame[i])
			if d < 0 {
				d = -d
			}
			if d > DupPixelGrayThresh {
				sig++
			}
		}
		sum += float64(sig) / float64(len(ia.frame))
		n++
	}
	ia.close()
	ib.close()
	if n == 0 {
		return 0, fmt.Errorf("两边都读不出帧")
	}
	return sum / float64(n), nil
}

// frameIter 逐帧读 ffmpeg 的灰度输出。
// 视频按 fps=5 抽样、最多前 VideoSampleSecs 秒；图片只有 1 帧。
// detail 尺寸（1024×1024）下一帧 1MB，必须流式逐帧算完就丢，不能整段进内存。
type frameIter struct {
	cmd   *exec.Cmd
	pipe  io.ReadCloser
	frame []byte
	err   error // io.EOF = 正常放完；其他 = ffmpeg 半路挂了
	n     int   // 已读出的帧数
}

func (it *frameIter) next() bool {
	_, err := io.ReadFull(it.pipe, it.frame)
	if err != nil {
		it.err = err
		return false
	}
	it.n++
	return true
}

func (it *frameIter) close() {
	it.pipe.Close()
	_ = it.cmd.Wait()
}

// startFrameStream 启动 ffmpeg 灰度帧流。
// 视频多帧（全片每秒 2 帧），图片单帧。调用方用完必须 close()。
// hwMode 非空时视频用对应显卡解码（cuda/qsv/d3d11va）+ hwdownload 回读——**分工用**：
// QC 走显卡时 CPU 空出来给视频转码，互不抢（用户 2026-10-01 00:41 的分工方案）。
// 显卡解码失败会静默回退软解（-hwaccel 的标准行为），检测数值不受影响。
func (s *QCService) startFrameStream(ctx context.Context, src string, w, h int, isVideo bool) (*frameIter, error) {
	s.hwMu.Lock()
	mode := s.hwMode
	s.hwMu.Unlock()
	if isVideo && mode != "" {
		return s.openRun(ctx, src, w, h, isVideo, mode, nil)
	}
	return s.openRun(ctx, src, w, h, isVideo, "", nil)
}

// frameArgs 拼出「从 src 取 w×h 灰度裸帧」的完整 ffmpeg 参数。
// **探测和实跑必须共用它**：试跑和真检测走同一条命令，探测出的快慢才对实跑有意义。
// hw 非空 = 显卡解码（帧留显存）→ hwdownload 回读 NV12 → CPU 侧降帧/缩放。
// 回读是全帧的（fps 是 CPU 滤镜，只能排在回读之后），PCIe 流量是实打实的代价。
// frameArgs 拼灰度帧流的 ffmpeg 参数。
// ⚠️ limit 必须是**已经切开的**参数（[]string{"-t","90"}），不能是 "-t 90" 这种带空格的单串：
// 塞成单个 argv 元素 ffmpeg 会报 "Unrecognized option 't 90'" 直接退出，探测永远取不到帧，
// 还会伪装成"显卡探测不可用"这种看起来很合理的结论。2026-10-02 就是这个坑白跑了两轮大跑。
func (s *QCService) frameArgs(src string, w, h int, isVideo bool, hw string, limit []string) []string {
	var vf string
	args := []string{"-y", "-v", "error"}
	if hw != "" {
		args = append(args, "-hwaccel", hw, "-hwaccel_output_format", hw)
		vf = "hwdownload,format=nv12,"
	}
	args = append(args, "-i", src)
	if isVideo {
		// fps 滤镜全片每秒挑 2 帧（用户 2026-10-01 21:17 定：全片都要抽，不限前 30 秒）；
		// 单帧误报会被全片样本的投票稀释掉。
		vf += fmt.Sprintf("fps=%g,", float64(VideoSampleFPS))
	}
	vf += fmt.Sprintf("format=gray,scale=%d:%d", w, h)
	args = append(args, "-vf", vf)
	if len(limit) > 0 {
		args = append(args, limit...)
	}
	return append(args, "-f", "rawvideo", "-pix_fmt", "gray", "-")
}

// openRun 按 frameArgs 起的帧流。limit 一般为空（全片），探测时传 "-t 90"。
// ⚠️ isVideo 必须如实传：fps 滤镜对单帧图片的输入会输出 0 帧（实测 dup1.jpg 0 字节），
// 写死 true 会让**所有图片**一帧都取不到 → 全部判损坏（flags=2）。2026-10-02 踩过。
func (s *QCService) openRun(ctx context.Context, src string, w, h int, isVideo bool, hw string, limit []string) (*frameIter, error) {
	if s.ffmpeg == "" {
		return nil, fmt.Errorf("ffmpeg 不可用")
	}
	args := s.frameArgs(src, w, h, isVideo, hw, limit)
	cmd := exec.CommandContext(ctx, s.ffmpeg, args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // 解码错误从"一帧都读不到/提前 EOF"体现，stderr 吞掉
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	it := &frameIter{cmd: cmd, pipe: pipe, frame: make([]byte, w*h)}
	return it, nil
}

// hwCandidates 按用户的选择返回"这次要试哪几条解码路"。
//
//	"cpu"/"n"              → 空切片 = 强制软解，一条显卡路都不试
//	"cuda"/"qsv"/"d3d11va" → 只试点名的这些（逗号分隔可多点，如 "cuda,qsv"）
//	"auto"/"y"/空           → 三条都试，交给下面的速度择优
//
// 2026-10-04 起拆细：以前只有 y/n 两档，想指定用哪张卡只能自己改代码。
func hwCandidates(choice string) []string {
	switch n := normChoice(choice); n {
	case "cpu":
		return nil
	case "auto":
		return []string{"cuda", "qsv", "d3d11va"}
	default: // 归一后的逗号列表（已排序去重）
		return strings.Split(n, ",")
	}
}

// normChoice 把命令行/接口给的选择归一：cpu / auto / 逗号分隔的候选集（排序去重）。
// 空串和 "y" 都当 auto（老行为：y = 让程序自己择优）。
// 存探测结论时也要用它 —— 候选集不同的结论不能互相复用。
func normChoice(choice string) string {
	s := strings.ToLower(strings.TrimSpace(choice))
	switch s {
	case "", "y", "yes", "auto":
		return "auto"
	case "n", "no", "cpu":
		return "cpu"
	}
	var set []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		var c string
		switch strings.TrimSpace(p) {
		case "cuda", "nvenc":
			c = "cuda"
		case "qsv":
			c = "qsv"
		case "d3d11va", "amf":
			c = "d3d11va"
		default:
			continue
		}
		if !seen[c] {
			seen[c] = true
			set = append(set, c)
		}
	}
	if len(set) == 0 {
		return "auto" // 认不出来 = 老行为，自己择优
	}
	sort.Strings(set)
	return strings.Join(set, ",")
}

// ProbeHWAccel 视频 QC 前的显卡探测：按 hwChoice 决定启用哪些路，候选依次试
// cuda（NVIDIA）→ qsv（Intel 核显）→ d3d11va（AMD/通用）。用户点名了某一条
// 就只试那一条；都没点（auto）才三条都试、比谁快。
// 全部失败 → 回退 CPU 软解（检测数值不变）。结果缓存，只探一次。
func (s *QCService) ProbeHWAccel(ctx context.Context, src string, choice string) (string, bool) {
	s.hwMu.Lock()
	if s.hwProbed {
		mode := s.hwMode
		s.hwMu.Unlock()
		return mode, mode != ""
	}
	s.hwMu.Unlock()

	// 持久化复用：上一次跑出来的结论（含"显卡更快/更慢"的结论）直接拿来用。
	// 只认**同一台机器**、没过期（驱动/显卡换了，7 天内重新试跑一次），
	// 而且必须是**同一套选择** —— 上次 auto 三条都试挑中了 qsv，这次用户点名 cuda，
	// 那条结论不适用于这次（候选集都不一样），必须重探。
	if len(hwCandidates(choice)) > 0 {
		if mode, at, ok := s.loadHWPick(normChoice(choice)); ok {
			s.hwMu.Lock()
			s.hwMode, s.hwProbed, s.hwPickedAt = mode, true, at
			s.hwMu.Unlock()
			what := hwName(mode)
			switch mode {
			case "":
				log.Printf("QC: 复用上次探测结论：显卡更慢，用 CPU 软解（%s 测的）", at.Format("01-02 15:04"))
			default:
				log.Printf("QC: 复用上次探测结论：%s（%s 测的）", what, at.Format("01-02 15:04"))
			}
			return mode, mode != ""
		}
	}

	// 速度择优：CPU 也当候选参与计时。只测"能不能用"会选上慢的那条 ——
	// 实测本机 QSV 能用但 QC 要 15 秒，CPU 软解只要 8 秒（初始化+回读开销），
	// 而 4080 这类强卡上 NVDEC 会明显快于 CPU。让每种硬件跑同样的素材比耗时，
	// 只有比当前最快者快 5% 以上才换（避免噪声抖动）。
	mode := ""
	best, cpuOK, cpuSec := 0.0, false, 0.0
	// picked：是否已经选出一个候选（CPU 也算一个）。
	// ⚠️ 不能拿 `mode == ""` 当"还没选过"——CPU 那一轮本来就把 mode 置成空串（表示"用 CPU"），
	// 显卡循环里 `mode == "" || ...` 会恒真，于是 **任何一条慢的显卡路都会被直接选中**：
	// 本机实测 CPU 5.8s / QSV 40.6s，却打出「择优 → 核显(QSV)（40.6 秒，比 CPU 的 5.8 秒快）」。
	bestHW := 0.0 // 最快那条**显卡**路的耗时；0 = 一条都没跑出帧
	picked := false
	cands := hwCandidates(choice)
	if s.ffmpeg != "" && len(cands) > 0 {
		if sec, ok := s.timeProbe(ctx, src, ""); ok {
			mode, best, cpuOK, cpuSec, picked = "", sec, true, sec, true
		}
		for _, hw := range cands {
			sec, ok := s.timeProbe(ctx, src, hw)
			if !ok {
				continue
			}
			if bestHW == 0 || sec < bestHW*0.95 {
				bestHW = sec
			}
			if !picked || sec < best*0.95 {
				mode, best, picked = hw, sec, true
			}
		}
	}
	switch {
	case mode != "":
		log.Printf("QC: 解码速度择优 → %s（%.1f 秒 / 前 90 秒试跑，比 CPU 的 %.1f 秒快）", hwName(mode), best, cpuSec)
	case cpuOK && bestHW > 0:
		log.Printf("QC: 试过的 %d 条显卡解码都更慢（最快 %.1f 秒）→ 视频 QC 用 CPU 软解（%.1f 秒）",
			len(cands), bestHW, cpuSec)
	case cpuOK:
		log.Printf("QC: 试过的 %d 条显卡解码都没取到帧（大概率静默回退软解），视频 QC 用 CPU 软解（%.1f 秒）",
			len(cands), cpuSec)
	default:
		log.Printf("QC: 解码探测整个不出帧（CPU 软解和三种显卡都不行），视频 QC 回退 CPU 软解（%s）", filepath.Base(src))
	}
	s.hwMu.Lock()
	s.hwMode, s.hwProbed, s.hwPickedAt = mode, true, time.Now()
	s.hwMu.Unlock()
	// 把结论记进库：下次启动不用再跑 4 段试跑（约 7 秒）。
	// "cpu"（mode 为空）也存 —— "显卡更慢"这个结论同样是硬知识，不该每次重测。
	// 连**当时的选择**一起存（hwSet）：换了个候选集就别复用旧结论，见 loadHWPick。
	if len(cands) > 0 {
		s.saveHWPick(mode, normChoice(choice))
	}
	return mode, mode != ""
}

// hwProbeKey / hwProbeMaxAge 硬件探测结论在库里的键和有效期。
const (
	hwProbeKey    = "qc_hw_pick"
	hwProbeMaxAge = 7 * 24 * time.Hour
)

// hwName 硬件类型的人话名（未知就原样返回）。
func hwName(mode string) string {
	switch mode {
	case "":
		return "CPU 软解"
	case "cuda":
		return "独显(NVDEC)"
	case "qsv":
		return "核显(QSV)"
	case "d3d11va":
		return "通用(D3D11VA)"
	}
	return mode
}

// loadHWPick 读回上次探测结论。格式 "mode\thost\tunix秒\t候选集"；
// 机器名不同（db 拷到别的电脑）、超时未更新、或**候选集与这次不一样**，一律当没存过（要重新测）。
//
// 末段是 2026-10-04 加的：以前只有 y/n，候选集恒定；拆细之后"auto 挑中 qsv"这条结论
// 不能拿来回答"点名 cuda"（候选集不同，结论可能压根没测过 cuda）。
// 老记录（只有 3 段）按 auto 处理。
func (s *QCService) loadHWPick(want string) (string, time.Time, bool) {
	if s.ms == nil {
		return "", time.Time{}, false
	}
	host, _ := os.Hostname()
	f := strings.SplitN(strings.TrimSpace(s.ms.MetaGet(hwProbeKey)), "\t", 4)
	if len(f) < 3 {
		return "", time.Time{}, false
	}
	switch f[0] {
	case "", "cpu": // "cpu" 与空串同义：都表示"用 CPU 软解"
	case "cuda", "qsv", "d3d11va":
	default:
		return "", time.Time{}, false
	}
	got := "auto" // 没写第 4 段的老记录 = 当时是 auto 三条都试
	if len(f) == 4 {
		got = normChoice(f[3])
	}
	if got != want {
		return "", time.Time{}, false
	}
	if f[1] != host {
		return "", time.Time{}, false
	}
	ts, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	at := time.Unix(ts, 0)
	if time.Since(at) > hwProbeMaxAge {
		return "", time.Time{}, false
	}
	mode := f[0]
	if mode == "cpu" {
		mode = ""
	}
	return mode, at, true
}

// saveHWPick 把本次结论写进库（写入失败不影响检测，只少一次缓存）。
func (s *QCService) saveHWPick(mode, hwSet string) {
	if s.ms == nil {
		return
	}
	host, _ := os.Hostname()
	if mode == "" {
		mode = "cpu" // 空串写进去读回来会被当成空，存个明确的记号
	}
	if err := s.ms.MetaSet(hwProbeKey, fmt.Sprintf("%s\t%s\t%d\t%s", mode, host, time.Now().Unix(), hwSet)); err != nil {
		log.Printf("QC: 硬件探测结论写库失败（下次会重测）: %v", err)
	}
}

// timeProbe 用同一段素材试跑某种硬件（hw="" 即 CPU 软解），**跑完两遍再计时**——
// 跑的内容和真检测一模一样（64 灰度一遍 + 1024 细节一遍，都限前 90 秒）。
//
// 为什么必须跑"真管道"、不能只试跑几十帧：
//  1. GPU 路的 hwdownload 排在 fps 之前，会把**每一帧整帧**回读（90 秒 1080p
//     ≈ 2700 帧 × 3MB ≈ 8GB 往内存搬）。这个成本是跟着"解码了多少帧"走的，
//     只取 60 帧的样本完全测不出来 —— 于是 30 秒单遍样本会误判"核显更快"，
//     实跑 7.8 分钟全片时 QSV 反而比 CPU 慢一倍（实测 68s vs 36s）。
//  2. 只跑一遍 64 灰度也会低估：两遍的解码量是一倍关系，显卡的初始化和回读
//     开销要在两遍的尺度上才看得准。
// 代价是一次探测多跑几秒，结论已经写库（7 天有效），不会每次启动都付。
// ⚠️ 容错口径：**两遍里只要有一遍真的取出过帧，就算这条路可跑**（返回 total>0）。
//
// 之前写成「任一遍 err 或 0 帧就整个 false」，结果 bigrun（13 个真实班级视频）里
// 1024 细节那遍在某些片子上起不来（GPU 路尤甚：hwdownload+format=nv12 在长片尾部
// 偶发取不到帧），整条路被判"用不了"，日志还打出「一条路都出不了帧」——其实 CPU 路
// 根本没被跑过，是假象。某一遍失败只影响那一遍采样，另一遍仍在工作，不该全盘否掉。
//
// 反过来也不能只看 err：0 帧说明管道压根没出图（hwaccel 被静默忽略的典型症状），
// 所以 err 和 n==0 同样只是 **continue 跳过这一遍**，而不是终止整次探测。
func (s *QCService) timeProbe(ctx context.Context, src, hw string) (float64, bool) {
	t0 := time.Now()
	total := 0
	for _, size := range []int{GraySize, DetailSize} {
		it, err := s.openRun(ctx, src, size, size, true, hw, []string{"-t", "90"})
		if err != nil {
				continue
		}
		n := 0
		for it.next() {
			n++
		}
		it.close()
		total += n
	}
	el := time.Since(t0).Seconds()
	if total == 0 {
		// 一次探测跑 4 段（CPU+三卡 × 两遍）全 0 帧 = 管道根本没起来。
		// 静默回退 CPU 会把「我自己写错了参数」永久伪装成「显卡探测不可用」，必须留痕。
		log.Printf("QC: 解码探测 %s 一帧都没取到（hw=%q，ffmpeg 参数 %v）→ 该路判为不可用",
			filepath.Base(src), hw, s.frameArgs(src, GraySize, GraySize, true, hw, []string{"-t", "90"}))
	}
	return el, total > 0
}


// Run 对单张图片跑全部检测器。返回 (nil, nil) 表示跳过（无 ffmpeg）。
// 重复判定需跨文件比对，这里只算 hash；跨文件聚合在阶段2 落库后做。
func (s *QCService) Run(ctx context.Context, src string) (*QCResult, error) {
	return s.RunMedia(ctx, src, false)
}

// RunMedia 对素材跑全部检测器，isVideo 时先抽代表帧。
// 视频和图片共用同一套判据：抽出来的那一帧就是一张图。
func (s *QCService) RunMedia(ctx context.Context, src string, isVideo bool) (*QCResult, error) {
	if s.ffmpeg == "" {
		return nil, nil
	}
	if isVideo {
		return s.runVideo(ctx, src)
	}
	return s.runImage(ctx, src)
}

// runImage 图片：单帧，检测器命中即置位（和原来的行为一致）。
func (s *QCService) runImage(ctx context.Context, src string) (*QCResult, error) {
	it, err := s.startFrameStream(ctx, src, GraySize, GraySize, false)
	if err != nil {
		return &QCResult{Flags: FlagCorrupted, Corrupted: true}, nil
	}
	if !it.next() {
		it.close()
		// 一帧都解不出来 → 损坏
		return &QCResult{Flags: FlagCorrupted, Corrupted: true}, nil
	}
	gray := make([]byte, len(it.frame))
	copy(gray, it.frame)
	it.close()
	return s.judgeFrame(ctx, gray, src), nil
}

// judgeFrame 对**单帧**跑全部检测器（图片路径直接用；视频路径逐帧调用做统计）。
// blurExtra/detail 流的结果由调用方传入（nil = 图片路径，正常单帧流程）。
func (s *QCService) judgeFrame(ctx context.Context, gray []byte, src string) *QCResult {
	r := &QCResult{}
	if blank, std := detectBlank(gray, GraySize); blank {
		r.Flags |= FlagBlank
		r.Blank = &DetectorOut{Conf: std}
	}
	r.GrayStd = stdOfGray(gray)
	if boxes, conf := detectLensDirt(gray, GraySize); len(boxes) > 0 {
		r.Flags |= FlagLensDirt
		r.LensDirt = &LensDirtOut{Conf: conf, Boxes: boxes}
	}
	// 曝光是直方图统计，与分辨率无关，用现成的 64 灰度即可。
	// 指标本身总是记下来（校准/排查要看），但只有越过阈值才挂 Exposure 对象和位掩码。
	if exp := detectExposure(gray, GraySize); exp != nil {
		r.ExpMean, r.ExpClipLow, r.ExpClipHigh = exp.Mean, exp.ClipLow, exp.ClipHigh
		if exp.Under || exp.Over {
			r.Flags |= FlagExposure
			r.Exposure = exp
		}
	}
	r.DupHash = fmt.Sprintf("%016x", pHash(gray, GraySize))

	// blur/noise 必须看细节：64×64 相当于对原图做 8~60 倍降采样，
	// 会把高频细节和噪点一起平均掉 —— 另取一帧高分辨率灰度专门给这两个检测器。
	// （视频路径：runVideo 先按帧统计投票，不走到这里。）
	{
		// 图片路径：另取一帧高分辨率灰度专门给模糊/噪点。
		// 64×64 相当于对原图做 8~60 倍降采样，会把高频细节和噪点一起平均掉。
		if detail, dw, dh, derr := s.detailGray(ctx, src, false); derr == nil {
			lap := lapStdOf(detail, dw, dh)
			r.LapStd = lap
			if lap < BlurLapStdThresh && r.Flags&FlagBlank == 0 {
				r.Flags |= FlagBlur
				r.Blur = &DetectorOut{Conf: lap}
			}
			floor := noiseFloorOf(detail, dw, dh)
			r.NoiseFloor = floor
			if floor > NoiseFloorThresh {
				r.Flags |= FlagNoise
				r.Noise = &DetectorOut{Conf: floor}
			}
		} else {
			// 必须留痕：静默跳过会让"没测成"看起来跟"测过、没问题"一模一样
			log.Printf("QC: %s 细节灰度(%d)提取失败，跳过模糊/噪点检测: %v", src, DetailSize, derr)
		}
	}
	return r
}

// detailResult 一帧高分辨率灰度（模糊/噪点检测用）。
type detailResult struct {
	buf []byte
	w   int
	h   int
}

// startOneStream 一次解码、只输出一路（2026-10-02 提速第一档）：全片帧缩放到
// 高清灰度后写入临时文件，64 灰度由 Go 降采样得到。
// 以前是各起一个 ffmpeg、各把全片解码一遍（两遍解码）；试过"一次解码两路输出"
// （64 走管道 + 1024 走文件），但 ffmpeg 两路各自跑 fps 滤镜、帧序列不同步

// downsampleBox 把 w×h 灰度用盒式平均降采样到 out×out（替代 ffmpeg 的第二次缩放）。

// runVideo 视频：全片每秒抽 2 帧（用户 2026-10-01 21:17 定），逐帧跑检测器，
// **同一问题命中超过 30% 的样本帧才报**。
// 单帧的误报——一帧过暗、一帧虚焦、一个转场——都会被其余正常帧稀释掉。
// 损坏不参与投票：解码失败（一帧都出不来）直接判损坏。
// 抖动检测走同一股帧流：每帧留行/列投影，流结束后对相邻帧做互相关估计
// 整体平移，偏移的往复震荡强度超阈值且方向反复翻转才报（单向摇摄不算）。
func (s *QCService) runVideo(ctx context.Context, src string) (*QCResult, error) {
	r := &QCResult{}

	// ---- 第一遍：64×64 灰度流（空镜/脏污/曝光/重复）----
	// 注：2026-10-02 试过"一次解码两路输出"和"单路输出 + Go 降采样"两种合并方案，
	// 前者两路 fps 滤镜帧序列不同步（抖动 amp 从 11 漂到 8），后者帧序列不对
	// （个别视频直接判损坏）。两次都验证失败，所以维持两遍解码，别再合并。
	gi, err := s.startFrameStream(ctx, src, GraySize, GraySize, true)
	if err != nil {
		return &QCResult{Flags: FlagCorrupted, Corrupted: true}, nil
	}
	n := 0
	blankHits, dirtHits, expHits, underHits, overHits := 0, 0, 0, 0, 0
	var dirtBoxes []Box
	var dirtConf float64
	var expMeanSum, expLowSum, expHighSum, stdSum float64
	expN, first := 0, true
	blankFlags := []bool{} // 逐帧空镜标记（帧序与 detail 流对齐，blur 判定用）
	for gi.next() {
		n++
		frame := make([]byte, len(gi.frame))
		copy(frame, gi.frame)
		if first {
			r.DupHash = fmt.Sprintf("%016x", pHash(frame, GraySize))
			first = false
		}
		blank, _ := detectBlank(frame, GraySize)
		blankFlags = append(blankFlags, blank)
		if blank {
			blankHits++
		}
		stdSum += stdOfGray(frame)
		if boxes, conf := detectLensDirt(frame, GraySize); len(boxes) > 0 {
			dirtHits++
			dirtBoxes, dirtConf = boxes, conf
		}
		if exp := detectExposure(frame, GraySize); exp != nil {
			expMeanSum += exp.Mean
			expLowSum += exp.ClipLow
			expHighSum += exp.ClipHigh
			expN++
			if exp.Under {
				underHits++
				expHits++
			} else if exp.Over {
				overHits++
				expHits++
			}
		}
	}
	gi.close()
	r.GrayStd = stdSum / float64(max(n, 1))

	if n == 0 {
		// 一帧都解不出来 → 损坏（不参与 30% 投票）
		return &QCResult{Flags: FlagCorrupted, Corrupted: true}, nil
	}

	// ---- 第二遍：高分辨率细节流（模糊/噪点 + 抖动投影），逐帧算完就丢 ----
	blurHits, noiseHits := 0, 0
	blurSamples := 0 // blur/noise 的抽稀样本数（每 5 帧 1 个）
	var lapSum, floorSum float64
	// 抖动检测用 detail 流的投影：1024×1024 上平移幅度是 64 灰度的 16 倍，
	// 区分度足够（64 上合成抖动 amp 只有 1.24，和三脚架噪声 0.79 几乎分不开）。
	rowProjs, colProjs := [][]float64{}, [][]float64{}
	di, derr := s.startFrameStream(ctx, src, DetailSize, DetailSize, true)
	if derr != nil {
		log.Printf("QC: %s 细节灰度(%d)提取失败，跳过模糊/噪点检测: %v", src, DetailSize, derr)
	} else {
		dw, dh := DetailSize, DetailSize
		idx := 0
		for di.next() {
			// 抖动投影：每帧都算（O(w+h) 便宜，抖动是高频信号不能漏）
			rp := make([]float64, dh)
			cp := make([]float64, dw)
			for y := 0; y < dh; y++ {
				var rs float64
				base := y * dw
				for x := 0; x < dw; x++ {
					v := float64(di.frame[base+x])
					rs += v
					cp[x] += v
				}
				rp[y] = rs / float64(dw)
			}
			for x := 0; x < dw; x++ {
				cp[x] /= float64(dh)
			}
			rowProjs = append(rowProjs, rp)
			colProjs = append(colProjs, cp)
			// 模糊/噪点：**每 5 帧抽 1**（O(w×h) 贵 500 倍，是视频 QC 的九成耗时）。
			if idx%5 == 0 {
				lap := lapStdOf(di.frame, dw, dh)
				floor := noiseFloorOf(di.frame, dw, dh)
				lapSum += lap
				floorSum += floor
				blank := idx < len(blankFlags) && blankFlags[idx]
				if lap < BlurLapStdThresh && !blank {
					blurHits++
				}
				if floor > NoiseFloorThresh {
					noiseHits++
				}
				blurSamples++
			}
			idx++
		}
		di.close()
		if blurSamples > 0 {
			r.LapStd = lapSum / float64(blurSamples)
			r.NoiseFloor = floorSum / float64(blurSamples)
		}
	}

	// ---- 抖动检测：投影互相关估计相邻帧的整体平移，统计震荡强度 ----
	// 手抖 = 偏移往复震荡（dv 大且方向反复翻转）；摇摄 = 偏移单向匀速走，不算抖。
	if len(rowProjs) >= 8 {
		offs := make([][2]float64, 0, len(rowProjs)) // 每帧相对前一帧的 (dx, dy)
		for i := 1; i < len(rowProjs); i++ {
			dy, qy := shiftEstimate(rowProjs[i-1], rowProjs[i], 8)
			dx, qx := shiftEstimate(colProjs[i-1], colProjs[i], 8)
			if qy > ShakeCutDiff || qx > ShakeCutDiff {
				// 两帧内容差异过大（场景切换/大动作），平移估计不可靠 → 沿用上一帧
				if len(offs) > 0 {
					offs = append(offs, offs[len(offs)-1])
				} else {
					offs = append(offs, [2]float64{})
				}
				continue
			}
			offs = append(offs, [2]float64{float64(dx), float64(dy)})
		}
		// amp 用 |dv| 的**中位数**而不是 RMS：动作帧会产生离群的伪偏移尖峰
		// （远景里运动员跑动主导投影变化），RMS 会被尖峰抬起来把三脚架素材
		// 也判成抖；中位数只看"多数帧之间的偏移变化"——真抖动是全程的，尖峰是少数。
		var dvsX, dvsY []float64
		flips, flipsN := 0, 0
		var prevX, prevY float64
		for i := 1; i < len(offs); i++ {
			dvx := offs[i][0] - offs[i-1][0]
			dvy := offs[i][1] - offs[i-1][1]
			dvsX = append(dvsX, math.Abs(dvx))
			dvsY = append(dvsY, math.Abs(dvy))
			if i > 1 {
				flipsN++
				if (dvx != 0 && prevX != 0 && dvx*prevX < 0) || (dvy != 0 && prevY != 0 && dvy*prevY < 0) {
					flips++
				}
			}
			prevX, prevY = dvx, dvy
		}
		if m := len(offs); m > 2 && len(dvsX) > 2 {
			ampX, ampY := medianAbs(dvsX), medianAbs(dvsY)
			amp := math.Max(ampX, ampY)
			flipRate := 0.0
			if flipsN > 0 {
				flipRate = float64(flips) / float64(flipsN)
			}
			log.Printf("QC 抖动: %s 帧=%d amp=%.2f flip=%.0f%%", filepath.Base(src), len(rowProjs), amp, flipRate*100)
			if amp > ShakeRmsThresh && flipRate > ShakeFlipRate {
				r.Flags |= FlagShake
				r.Shake = &ShakeOut{Amp: amp}
			}
		}
	}

	// ---- 投票：命中超过 30% 的样本帧才置位 ----
	ratio := func(hits int) float64 { return float64(hits) / float64(n) }
	if ratio(blankHits) > VideoIssueRatio {
		r.Flags |= FlagBlank
		r.Blank = &DetectorOut{Conf: ratio(blankHits)}
	}
	if ratio(dirtHits) > VideoIssueRatio {
		r.Flags |= FlagLensDirt
		r.LensDirt = &LensDirtOut{Conf: dirtConf, Boxes: dirtBoxes}
	}
	if expN > 0 {
		r.ExpMean = expMeanSum / float64(expN)
		r.ExpClipLow = expLowSum / float64(expN)
		r.ExpClipHigh = expHighSum / float64(expN)
	}
	if ratio(expHits) > VideoIssueRatio {
		r.Flags |= FlagExposure
		r.Exposure = &ExposureOut{
			Under: underHits >= overHits, Over: overHits > underHits,
			Mean: r.ExpMean, ClipLow: r.ExpClipLow, ClipHigh: r.ExpClipHigh,
		}
	}
	if blurSamples > 0 {
		br := float64(blurHits) / float64(blurSamples)
		nr := float64(noiseHits) / float64(blurSamples)
		if br > VideoIssueRatio {
			r.Flags |= FlagBlur
			r.Blur = &DetectorOut{Conf: r.LapStd}
		}
		if nr > VideoIssueRatio {
			r.Flags |= FlagNoise
			r.Noise = &DetectorOut{Conf: r.NoiseFloor}
		}
	}
	return r, nil
}

// Detail 返回 JSON 字符串（写库用）。
func (r *QCResult) Detail() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// ---- 纯算法检测器 ----

// shiftEstimate 一维投影互相关：找 dx ∈ [-maxShift, maxShift] 使两投影错位叠加
// 的平均差最小。返回 (偏移, 匹配平均差)——匹配差大 = 两帧内容对不上（场景切换）。
func shiftEstimate(a, b []float64, maxShift int) (int, float64) {
	// 3 点移动平均压压缩噪声：噪声让投影毛刺丛生，互相关会在 ±1px 里乱选
	a = smooth3(a)
	b = smooth3(b)
	best, bestD := 0, math.MaxFloat64
	for dx := -maxShift; dx <= maxShift; dx++ {
		var d float64
		cnt := 0
		for i := range a {
			j := i + dx
			if j < 0 || j >= len(b) {
				continue
			}
			d += math.Abs(a[i] - b[j])
			cnt++
		}
		if cnt == 0 {
			continue
		}
		d /= float64(cnt)
		if d < bestD {
			bestD, best = d, dx
		}
	}
	return best, bestD
}

// medianAbs 绝对值切片的中位数（对少数离群帧稳健）。
func medianAbs(v []float64) float64 {
	c := make([]float64, len(v))
	copy(c, v)
	sort.Float64s(c)
	return c[len(c)/2]
}

// smooth3 三点移动平均（端点保持原值），压投影曲线的压缩噪声毛刺。
func smooth3(v []float64) []float64 {
	out := make([]float64, len(v))
	copy(out, v)
	for i := 1; i < len(v)-1; i++ {
		out[i] = (v[i-1] + v[i] + v[i+1]) / 3
	}
	return out
}

// detectBlank 全局灰度标准差极低（全黑/全白/纯色）判空镜。
func detectBlank(buf []byte, q int) (bool, float64) {
	var sum, sum2 float64
	n := float64(len(buf))
	for _, b := range buf {
		v := float64(b)
		sum += v
		sum2 += v * v
	}
	mean := sum / n
	variance := sum2/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	std := math.Sqrt(variance)
	return std < BlankStdThresh, std
}

// stdOfGray 全局灰度标准差。
func stdOfGray(buf []byte) float64 {
	var sum, sum2 float64
	n := float64(len(buf))
	for _, b := range buf {
		v := float64(b)
		sum += v
		sum2 += v * v
	}
	mean := sum / n
	variance := sum2/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}

// detectExposure 曝光异常检测（欠曝 / 过曝）。
//
// 为什么不能只看均值：夜景本来就暗、雪地本来就亮，那是**正常**曝光。
// 真正的欠曝/过曝特征是直方图在 0 或 255 处堆出一个尖峰（暗部被压死 / 高光糊掉），
// 所以判据是「近黑/近白像素占比过高」**并且**整体确实偏暗/偏亮 —— 两个条件一起才成立。
func detectExposure(buf []byte, q int) *ExposureOut {
	var sum float64
	low, high := 0, 0
	n := len(buf)
	for _, b := range buf {
		sum += float64(b)
		if int(b) <= ExpClipLowValue {
			low++
		}
		if int(b) >= ExpClipHighValue {
			high++
		}
	}
	mean := sum / float64(n)
	out := &ExposureOut{
		Mean:     mean,
		ClipLow:  float64(low) / float64(n),
		ClipHigh: float64(high) / float64(n),
	}
	if mean < ExpUnderMean && out.ClipLow > ExpClipLowThresh {
		out.Under = true
	}
	if mean > ExpOverMean && out.ClipHigh > ExpClipHighThresh {
		out.Over = true
	}
	return out
}

// lapStdOf 拉普拉斯响应的标准差（越大越锐利）。
// 模糊 = 高频缺失 = 边缘变缓 = 拉普拉斯响应整体变小，所以这个值和锐度正相关。
// 支持任意 w×h（细节提取是保持宽高比的，不一定是正方形）。
func lapStdOf(buf []byte, w, h int) float64 {
	var sum, sum2 float64
	cnt := 0
	for y := 1; y < h-1; y++ {
		row := y * w
		for x := 1; x < w-1; x++ {
			i := row + x
			l := 4*float64(buf[i]) - float64(buf[i-1]) - float64(buf[i+1]) -
				float64(buf[i-w]) - float64(buf[i+w])
			sum += l
			sum2 += l * l
			cnt++
		}
	}
	if cnt == 0 {
		return 0
	}
	mean := sum / float64(cnt)
	v := sum2/float64(cnt) - mean*mean
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// noiseFloorOf 噪声底线：所有块标准差里**最平坦那 10%** 的中位数。
//
// 思路：再干净的图也有成片的平坦区（天空、墙面、皮肤），那里的波动**就是**噪声本身；
// 取最平坦的一批块来量标准差，就能把"结构纹理"排除掉、单独测出噪声。
// 注意别在 64×64 上做：降采样会把噪点平均掉（见 Run 里的说明）。
func noiseFloorOf(buf []byte, w, h int) float64 {
	// 块**边长固定**（不是块数固定）：这样不管图片多大、宽高比如何，
	// 每个块覆盖的像素数都一样，噪声统计才可比。
	cell := NoiseCell
	if w < cell*2 || h < cell*2 {
		return 0
	}
	gw, gh := w/cell, h/cell
	stds := make([]float64, 0, gw*gh)
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			var s, s2 float64
			cnt := 0
			y0, y1 := gy*cell, (gy+1)*cell
			x0, x1 := gx*cell, (gx+1)*cell
			if y1 > h {
				y1 = h
			}
			if x1 > w {
				x1 = w
			}
			for y := y0; y < y1; y++ {
				row := y * w
				for x := x0; x < x1; x++ {
					v := float64(buf[row+x])
					s += v
					s2 += v * v
					cnt++
				}
			}
			if cnt == 0 {
				continue
			}
			m := s / float64(cnt)
			v := s2/float64(cnt) - m*m
			if v < 0 {
				v = 0
			}
			stds = append(stds, math.Sqrt(v))
		}
	}
	sort.Float64s(stds)
	if len(stds) == 0 {
		return 0
	}
	idx := len(stds) / 10 // 第 10 百分位：最平坦的一批
	if idx >= len(stds) {
		idx = len(stds) - 1
	}
	return stds[idx]
}

// detectLensDirt 镜头脏污 / 局部遮挡检测。
//
// 思路（区别于简单“低方差块”）：真实的脏污/遮挡在灰度上是
//   - 内部平滑（低方差），且
//   - 相对周围邻域是“突兀的”亮/暗块（亮度异常）。
//
// 天空、墙面这类大面积平滑区虽平滑，但邻域同质 → 不判异常；地平线这类细线
// 因宽/高不足 2 块被排除。最终还要求脏点不覆盖整帧（局部性）。
func detectLensDirt(buf []byte, q int) ([]Box, float64) {
	grid := 8
	cell := q / grid
	means := make([]float64, grid*grid)
	stds := make([]float64, grid*grid)
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			var s, s2 float64
			cnt := 0
			for y := gy * cell; y < (gy+1)*cell; y++ {
				for x := gx * cell; x < (gx+1)*cell; x++ {
					v := float64(buf[y*q+x])
					s += v
					s2 += v * v
					cnt++
				}
			}
			m := s / float64(cnt)
			v := s2/float64(cnt) - m*m
			if v < 0 {
				v = 0
			}
			means[gy*grid+gx] = m
			stds[gy*grid+gx] = math.Sqrt(v)
		}
	}
	// 全局块方差分布：衡量整图纹理强度，用于设定“平滑”阈值。
	var gvSum, gvSum2 float64
	for _, s := range stds {
		gvSum += s
		gvSum2 += s * s
	}
	gn := float64(len(stds))
	gvMean := gvSum / gn
	gvVar := gvSum2/gn - gvMean*gvMean
	if gvVar < 0 {
		gvVar = 0
	}
	gvStd := math.Sqrt(gvVar)
	// 平滑阈值：低于此方差视为“疑似污点内部”。固定下限 4 避免把有纹理当平滑。
	smoothThresh := math.Max(4.0, 0.35*gvStd)
	// 亮度异常阈值（0-255 尺度固定值）：污点相对场景是突兀色块。
	// 用固定值而非按邻域方差缩放，避免污点边缘邻域双峰把阈值撑大导致漏检。
	const brightOutlier = 25.0

	type gridCell struct{ gy, gx int }
	var bad []gridCell
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			i := gy*grid + gx
			if stds[i] >= smoothThresh {
				continue // 自身不够平滑：不是污点内部
			}
			// 局部邻域（3x3 不含自身）的块均值
			var nSum float64
			nn := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					ny, nx := gy+dy, gx+dx
					if ny < 0 || ny >= grid || nx < 0 || nx >= grid {
						continue
					}
					nSum += means[ny*grid+nx]
					nn++
				}
			}
			if nn == 0 {
				continue
			}
			nMean := nSum / float64(nn)
			if math.Abs(means[i]-nMean) > brightOutlier {
				bad = append(bad, gridCell{gy, gx})
			}
		}
	}
	if len(bad) < LensDirtMinBlocks {
		return nil, 0
	}
	minx, miny, maxx, maxy := grid, grid, 0, 0
	for _, c := range bad {
		if c.gx < minx {
			minx = c.gx
		}
		if c.gx > maxx {
			maxx = c.gx
		}
		if c.gy < miny {
			miny = c.gy
		}
		if c.gy > maxy {
			maxy = c.gy
		}
	}
	bw := maxx - minx + 1
	bh := maxy - miny + 1
	// 排除细线（如地平线）：成团的脏点宽高都应 ≥ 2 块
	if bw < 2 || bh < 2 {
		return nil, 0
	}
	// 局部性约束：脏点框面积占比应 < 0.6，避免整帧统一偏色被误判
	areaRatio := float64(bw*bh) / float64(grid*grid)
	if areaRatio >= 0.6 {
		return nil, 0
	}
	// 密度约束：脏斑块是**实心**的（异常块填满包围盒的一半以上）。
	// 灰色天空与地面/建筑的交界是一条**稀疏的线**——包围盒被撑得很大
	// 但内部大多是正常块（实测天空图密度 12%~22%，验收真斑 100%）。
	density := float64(len(bad)) / float64(bw*bh)
	if density < 0.5 {
		return nil, 0
	}
	box := Box{
		X: float64(minx) / float64(grid),
		Y: float64(miny) / float64(grid),
		W: float64(bw) / float64(grid),
		H: float64(bh) / float64(grid),
	}
	return []Box{box}, float64(len(bad)) / gn
}

// pHash 计算 64-bit 感知哈希（DCT 低频二值化）。
func pHash(buf []byte, q int) uint64 {
	N := 64
	if q < N {
		N = q
	}
	m := make([]float64, N*N)
	for i := 0; i < N*N && i < len(buf); i++ {
		m[i] = float64(buf[i])
	}
	d := dct2D(m, N)
	// 取左上 8×8 低频块（DCT 能量集中在低频，即矩阵的左上角）
	low := make([]float64, 8*8)
	k := 0
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			low[k] = d[r*N+c]
			k++
		}
	}
	vals := make([]float64, 0, 8*8-1)
	for i := 1; i < 8*8; i++ {
		vals = append(vals, low[i]) // 排除 DC（low[0]）
	}
	med := median(vals)
	var h uint64
	for i := 0; i < 8*8; i++ {
		if low[i] > med {
			h |= 1 << uint(i)
		}
	}
	return h
}

// Hamming 两个 pHash 的汉明距离。
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

func dct1D(x []float64) []float64 {
	N := len(x)
	X := make([]float64, N)
	for k := 0; k < N; k++ {
		var s float64
		for n := 0; n < N; n++ {
			s += x[n] * math.Cos(math.Pi/float64(N)*(float64(n)+0.5)*float64(k))
		}
		c := 1.0
		if k == 0 {
			c = math.Sqrt(1 / float64(N))
		} else {
			c = math.Sqrt(2 / float64(N))
		}
		X[k] = c * s
	}
	return X
}

func dct2D(m []float64, N int) []float64 {
	tmp := make([]float64, N*N)
	row := make([]float64, N)
	for i := 0; i < N; i++ {
		copy(row, m[i*N:(i+1)*N])
		r := dct1D(row)
		copy(tmp[i*N:], r)
	}
	out := make([]float64, N*N)
	col := make([]float64, N)
	for j := 0; j < N; j++ {
		for i := 0; i < N; i++ {
			col[i] = tmp[i*N+j]
		}
		c := dct1D(col)
		for i := 0; i < N; i++ {
			out[i*N+j] = c[i]
		}
	}
	return out
}

func median(vs []float64) float64 {
	s := make([]float64, len(vs))
	copy(s, vs)
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j] > key {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}
