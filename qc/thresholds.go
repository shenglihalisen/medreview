package qc

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// Thresholds 是可运行时覆盖的"是/否"判定阈值。
//
// ⚠️ 检测算法 / 测量过程零改动 —— 这里只决定"量出来的指标越过哪条线算中"。
// 默认值 = 原 qc.go 编译期校准常量（实拍校准、宁漏勿滥、留 2–3× 余量），
// 见 DefaultThresholds()；不调阈值时行为与旧版逐字节一致。
type Thresholds struct {
	BlurLapStd float64 `json:"blurLapStd"` // 拉普拉斯标准差 < 此值判模糊（越大越锐）
	NoiseFloor float64 `json:"noiseFloor"` // 平坦区噪声底线 > 此值判噪点
	BlankStd   float64 `json:"blankStd"`   // 全局灰度标准差 < 此值判空镜
	ExpUnder   float64 `json:"expUnder"`   // 平均亮度 < 此值 且近黑像素>35% 判欠曝
	ExpOver    float64 `json:"expOver"`    // 平均亮度 > 此值 且近白像素>80% 判过曝
	ShakeRms   float64 `json:"shakeRms"`   // 帧间平移RMS > 此值（且翻转率过半）判抖动
	DupSim     float64 `json:"dupSim"`     // 相似度：显著差异像素占比 ≤ 1-DupSim 判重复（0.95 ⇔ 差异≤5%）
}

// DefaultThresholds 校准默认值 = 原 qc.go 编译期常量。
func DefaultThresholds() Thresholds {
	return Thresholds{
		BlurLapStd: BlurLapStdThresh,
		NoiseFloor: NoiseFloorThresh,
		BlankStd:   BlankStdThresh,
		ExpUnder:   ExpUnderMean,
		ExpOver:    ExpOverMean,
		ShakeRms:   ShakeRmsThresh,
		DupSim:     1 - DupPixelDiffThresh, // 差异占比 0.05 ⇔ 相似度 0.95
	}
}

// ThresholdSpec 单个阈值的滑块范围与方向（后端钳制 + 前端滑块共用一份，不会两边脱节）。
type ThresholdSpec struct {
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Step   float64 `json:"step"`
	Higher string  `json:"higher"` // "strict"=调大更严（更多标红） / "loose"=调大更宽松（更少标红）
}

// ThresholdSpecs 各阈值的合法范围。范围按实拍分布 ±2–3× 设，拖不出物理不合理区。
func ThresholdSpecs() map[string]ThresholdSpec {
	return map[string]ThresholdSpec{
		"blurLapStd": {Min: 5, Max: 60, Step: 1, Higher: "strict"},
		"noiseFloor": {Min: 1, Max: 20, Step: 0.5, Higher: "loose"},
		"blankStd":   {Min: 2, Max: 30, Step: 0.5, Higher: "strict"},
		"expUnder":   {Min: 30, Max: 160, Step: 5, Higher: "strict"},
		"expOver":    {Min: 90, Max: 220, Step: 5, Higher: "loose"},
		"shakeRms":   {Min: 0.3, Max: 6, Step: 0.1, Higher: "loose"},
		"dupSim":     {Min: 0.5, Max: 1, Step: 0.01, Higher: "strict"},
	}
}

// ClampThresholds 把每个字段钳回合法范围。越界不报错（拖到头就是边界值），防呆。
func ClampThresholds(t Thresholds) Thresholds {
	s := ThresholdSpecs()
	cl := func(v float64, sp ThresholdSpec) float64 {
		if v < sp.Min {
			return sp.Min
		}
		if v > sp.Max {
			return sp.Max
		}
		return v
	}
	t.BlurLapStd = cl(t.BlurLapStd, s["blurLapStd"])
	t.NoiseFloor = cl(t.NoiseFloor, s["noiseFloor"])
	t.BlankStd = cl(t.BlankStd, s["blankStd"])
	t.ExpUnder = cl(t.ExpUnder, s["expUnder"])
	t.ExpOver = cl(t.ExpOver, s["expOver"])
	t.ShakeRms = cl(t.ShakeRms, s["shakeRms"])
	t.DupSim = cl(t.DupSim, s["dupSim"])
	return t
}

// VersionFor 返回"算法版本 + 阈值指纹"，写进 qc 表的 qc_version 列：
//
//   - 默认阈值 → 正好 QCVersion（与历史行为一致，默认阈值的结果可复用）；
//   - 任一阈值自定义 → QCVersion + 稳定指纹（≠ QCVersion）→ 旧行全部失配 →
//     HasQC 判无效 → 后台按新阈值重跑。
//
// 指纹 = 64 位 FNV 对 7 个字段比特串的哈希 mod 1e9，不同阈值集碰撞概率可忽略。
func VersionFor(t Thresholds) int {
	if t == DefaultThresholds() {
		return QCVersion
	}
	h := fnv.New64a()
	var b [8]byte
	for _, v := range []float64{t.BlurLapStd, t.NoiseFloor, t.BlankStd, t.ExpUnder, t.ExpOver, t.ShakeRms, t.DupSim} {
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
		h.Write(b[:])
	}
	v := QCVersion + int(h.Sum64()%1_000_000_000)
	if v == QCVersion { // 指纹撞出 0 的极端防御：保证与默认版可区分
		v = QCVersion + 1
	}
	return v
}
