package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"medreview/qc"
)

func toHex(h uint64) string { return fmt.Sprintf("%016x", h) }

// 行构造 helper：detail 可以传 ""（无脏污）或一段带 lensDirt.boxes 的 JSON。
func qcRow(id int64, flags int, hash uint64, hasHash bool, device, shoot, detail string) QCRow {
	h := ""
	if hasHash {
		h = toHex(hash)
	}
	return QCRow{FileID: id, Flags: flags, DupHash: h, Device: device, ShootKey: shoot, Detail: detail}
}

func dirtDetail(boxes ...qc.Box) string {
	if len(boxes) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`{"flags":8,"lensDirt":{"conf":0.1,"boxes":[`)
	for i, b := range boxes {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"x":%g,"y":%g,"w":%g,"h":%g}`, b.X, b.Y, b.W, b.H)
	}
	sb.WriteString("}]}")
	return sb.String()
}

func TestBoxIoU(t *testing.T) {
	a := qc.Box{X: 0, Y: 0, W: 0.5, H: 0.5}
	if v := a.IoU(a); v < 0.99 {
		t.Fatalf("自己和自己 IoU 应为 1: %v", v)
	}
	if v := a.IoU(qc.Box{X: 0.6, Y: 0.6, W: 0.3, H: 0.3}); v != 0 {
		t.Fatalf("不相交 IoU 应为 0: %v", v)
	}
	// 交集 0.25×0.25=0.0625，并集 0.25+0.25-0.0625=0.4375 → IoU=1/7≈0.1429
	if v := a.IoU(qc.Box{X: 0.25, Y: 0.25, W: 0.5, H: 0.5}); v < 0.14 || v > 0.145 {
		t.Fatalf("半重叠 IoU 应约 0.1429: %v", v)
	}
}

// 连拍组：同设备、拍摄时间相邻（间隔 ≤ 窗口）的三张，锚是 id 最小的；
// 与锚汉明 ≤5 的标重复，时间窗外的不管。
func TestReconcileBurstNearDuplicates(t *testing.T) {
	base := uint64(0x0F0F0F0F0F0F0F0F)
	near := base ^ 0b1011 // 汉明距离 3
	far := base ^ 0xFFFF000000000000
	rows := []QCRow{
		qcRow(1, 0, base, true, "CamX", "2026-09-26 10:00:00", ""),
		qcRow(2, 0, near, true, "CamX", "2026-09-26 10:00:01", ""),
		qcRow(3, 0, far, true, "CamX", "2026-09-26 10:00:02", ""),
	}
	plan, _ := reconcilePlan(rows)
	if got, ok := plan[2]; !ok || got&qc.FlagDup == 0 {
		t.Fatalf("连拍近似件应标重复: %v", plan)
	}
	if _, ok := plan[3]; ok {
		t.Fatalf("与锚差异大的不应被标: %v", plan)
	}
	if _, ok := plan[1]; ok {
		t.Fatalf("锚（最小 id）不应被标: %v", plan)
	}
}

// 时间窗外的不算同一簇：间隔超过窗口就互相独立，近似也不标。
func TestReconcileBurstWindowSplits(t *testing.T) {
	base := uint64(0x0F0F0F0F0F0F0F0F)
	near := base ^ 0b1011
	rows := []QCRow{
		qcRow(1, 0, base, true, "CamX", "2026-09-26 10:00:00", ""),
		qcRow(2, 0, near, true, "CamX", "2026-09-26 10:00:30", ""), // 离得远：不是连拍
	}
	if plan, _ := reconcilePlan(rows); len(plan) != 0 {
		t.Fatalf("时间窗外不应标重复: %v", plan)
	}
}

// 镜头污损：同设备 4 张（≥3 可确认），id=1/2 在同一位置都有脏点框 → 保留；
// id=3 的脏点位置独一无二 → 判为场景内容，清掉。
func TestReconcileDirtConfirm(t *testing.T) {
	same := qc.Box{X: 0.375, Y: 0.375, W: 0.25, H: 0.25}
	alone := qc.Box{X: 0.9, Y: 0.1, W: 0.08, H: 0.08}
	rows := []QCRow{
		qcRow(1, qc.FlagLensDirt, 0x11, true, "CamX", "", dirtDetail(same)),
		qcRow(2, qc.FlagLensDirt, 0x22, true, "CamX", "", dirtDetail(same)),
		qcRow(3, qc.FlagLensDirt, 0x33, true, "CamX", "", dirtDetail(alone)),
		qcRow(4, 0, 0x44, true, "CamX", "", ""),
	}
	plan, _ := reconcilePlan(rows)
	if got, ok := plan[1]; ok && got&qc.FlagLensDirt != 0 {
		t.Fatalf("跨照片确认的污点不应被清: %v", plan)
	}
	if got, ok := plan[3]; ok && got&qc.FlagLensDirt != 0 {
		t.Fatalf("单张独有的脏点应被清: %v", plan)
	}
	if f1 := (plan[1]); f1 != 0 && f1&qc.FlagLensDirt == 0 {
		t.Fatalf("id=1 不该出现在改动里: %v", plan)
	}
	// id=1 有别的照片同位置印证 → 不应出现在改动里（flags 不变）
	if _, ok := plan[1]; ok {
		t.Fatalf("确认过的污点 flags 不变，不该进改动表: %v", plan)
	}
}

// 参照不足（同设备只有 2 张）不做确认，维持单张判定。
func TestReconcileDirtNeedsEnoughRefs(t *testing.T) {
	b := qc.Box{X: 0.375, Y: 0.375, W: 0.25, H: 0.25}
	rows := []QCRow{
		qcRow(1, qc.FlagLensDirt, 0x11, true, "CamY", "", dirtDetail(b)),
		qcRow(2, 0, 0x22, true, "CamY", "", ""),
	}
	if plan, _ := reconcilePlan(rows); len(plan) != 0 {
		t.Fatalf("参照不足不应清脏污: %v", plan)
	}
}

// 损坏件不参与污点确认的参照（hash 无意义、画面也不可信）。
func TestReconcileCorruptNotARef(t *testing.T) {
	b := qc.Box{X: 0.375, Y: 0.375, W: 0.25, H: 0.25}
	rows := []QCRow{
		qcRow(1, qc.FlagLensDirt, 0x11, true, "CamZ", "", dirtDetail(b)),
		qcRow(2, 0, 0x22, true, "CamZ", "", ""),
		qcRow(3, qc.FlagCorrupted, 0, false, "CamZ", "", ""),
	}
	if plan, _ := reconcilePlan(rows); len(plan) != 0 {
		t.Fatalf("损坏件不应计入参照: %v", plan)
	}
}

// 真实素材的 EXIF 应能读出型号和拍摄时间（拿不到就说明解析器坏了）。
func TestExifReal(t *testing.T) {
	dir := os.Getenv("QC_DIR")
	if dir == "" {
		t.Skip("未设 QC_DIR，跳过")
	}
	var found int
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || found >= 1 {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".jpg") {
			return nil
		}
		device, shoot := exifCaptureTags(p)
		t.Logf("%s -> device=%q shoot=%q", filepath.Base(p), device, shoot)
		if device == "" || shoot == "" {
			t.Errorf("真实照片应能读出 Model 和 DateTimeOriginal: device=%q shoot=%q", device, shoot)
		}
		found++
		return nil
	})
	if found == 0 {
		t.Skip("目录里没有 jpg")
	}
}
