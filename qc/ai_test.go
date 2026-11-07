package qc

import (
	"os"
	"path/filepath"
	"testing"
)

// AI 检测器是外部进程（qcai.exe），这里只测"降级路径"——
// 真实推理需要 dll+模型，放在集成测试里，不塞进单元测试。

func TestNewAIDetectorRejectsEmptyPaths(t *testing.T) {
	if _, err := NewAIDetector("", "", ""); err == nil {
		t.Fatal("三个路径全空时应报错")
	}
	if _, err := NewAIDetector("only.exe", "", ""); err == nil {
		t.Fatal("缺 model/dll 时应报错")
	}
}

func TestNewAIDetectorRejectsMissingFiles(t *testing.T) {
	if _, err := NewAIDetector("nope.exe", "nope.onnx", "nope.dll"); err == nil {
		t.Fatal("文件不存在时应报错，而不是返回可用检测器")
	}
}

func TestNewAIDetectorRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "qcai.exe")
	mdl := filepath.Join(dir, "m.onnx")
	dll := filepath.Join(dir, "o.dll")
	for _, p := range []string{exe, mdl, dll} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewAIDetector(exe, mdl, dll); err == nil {
		t.Fatal("空文件应被拒绝")
	}
}

func TestNilAIDetectorScoreFile(t *testing.T) {
	var a *AIDetector
	if _, _, err := a.ScoreFile("whatever.jpg"); err == nil {
		t.Fatal("nil 检测器必须返回错误，调用方据此留痕跳过")
	}
}

// FlagAISuspect 必须落在问题掩码里，且不与其它位冲突。
func TestFlagAISuspectBit(t *testing.T) {
	if FlagAISuspect != 1<<8 {
		t.Fatalf("FlagAISuspect = %d, want %d", FlagAISuspect, 1<<8)
	}
	if FlagIssueMask&FlagAISuspect == 0 {
		t.Fatal("AI 怀疑应算质量问题（否则筛选看不到）")
	}
	for _, other := range []int{FlagDup, FlagCorrupted, FlagBlank, FlagLensDirt, FlagBlur, FlagExposure, FlagNoise, FlagShake} {
		if FlagAISuspect == other {
			t.Fatalf("FlagAISuspect 与已有位冲突: %d", other)
		}
	}
}

// 默认阈值必须落在 spec 范围内，否则前端滑块一打开就被钳到边界。
func TestAIThresholdsWithinSpec(t *testing.T) {
	th := DefaultThresholds()
	specs := ThresholdSpecs()
	for _, kv := range []struct {
		name string
		v    float64
	}{{"aiBlur", th.AIBlur}, {"aiNoise", th.AINoise}, {"aiDirt", th.AIDirt}} {
		sp, ok := specs[kv.name]
		if !ok {
			t.Fatalf("%s 缺少 spec", kv.name)
		}
		if kv.v < sp.Min || kv.v > sp.Max {
			t.Fatalf("%s=%v 超出 [%v,%v]", kv.name, kv.v, sp.Min, sp.Max)
		}
	}
	if th != ClampThresholds(th) {
		t.Fatal("默认阈值经过 Clamp 后不应变化")
	}
}
