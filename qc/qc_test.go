package qc

import (
	"context"
	"testing"
)

// makeGray 生成 q×q 灰度测试图（fn 决定每个像素值）。
func makeGray(q int, fn func(x, y int) byte) []byte {
	b := make([]byte, q*q)
	for y := 0; y < q; y++ {
		for x := 0; x < q; x++ {
			b[y*q+x] = fn(x, y)
		}
	}
	return b
}

func TestDetectBlank(t *testing.T) {
	black := makeGray(64, func(x, y int) byte { return 0 })
	if ok, _ := detectBlank(black, 64); !ok {
		t.Fatal("全黑应判空镜")
	}
	grad := makeGray(64, func(x, y int) byte { return byte(x * 4 % 256) })
	if ok, _ := detectBlank(grad, 64); ok {
		t.Fatal("渐变图不应判空镜")
	}
}

func TestPHashIdentical(t *testing.T) {
	fn := func(x, y int) byte { return byte((x + y) * 2 % 256) }
	a := makeGray(64, fn)
	b := makeGray(64, fn)
	ha, hb := pHash(a, 64), pHash(b, 64)
	if ha != hb {
		t.Fatalf("相同图 pHash 应相等: %016x vs %016x", ha, hb)
	}
	if Hamming(ha, hb) != 0 {
		t.Fatal("相同图汉明距离应为 0")
	}
}

func TestPHashDifferent(t *testing.T) {
	a := makeGray(64, func(x, y int) byte { return byte(x * 4) })
	b := makeGray(64, func(x, y int) byte { return byte(y * 4) })
	if Hamming(pHash(a, 64), pHash(b, 64)) == 0 {
		t.Fatal("明显不同的图汉明距离不应为 0")
	}
}

func TestDetectLensDirt(t *testing.T) {
	q := 64
	g := makeGray(q, func(x, y int) byte { return byte((x*7 + y*13) % 200) })
	// 中央 24×24 区域填均匀亮灰（低方差、且与背景均值明显不同），模拟镜头脏污/遮挡
	for y := 20; y < 44; y++ {
		for x := 20; x < 44; x++ {
			g[y*q+x] = 230
		}
	}
	boxes, conf := detectLensDirt(g, q)
	if len(boxes) == 0 {
		t.Fatalf("应检测到中央脏点块，conf=%v", conf)
	}
	bx := boxes[0]
	if bx.X > 0.5 || bx.Y > 0.5 || bx.X+bx.W < 0.3 || bx.Y+bx.H < 0.3 {
		t.Fatalf("脏点框应覆盖中央，实际 %+v", bx)
	}
}

func TestDetectExposure(t *testing.T) {
	q := 64
	// 欠曝：整帧压到死黑
	e := detectExposure(makeGray(q, func(x, y int) byte { return 0 }), q)
	if !e.Under || e.Over {
		t.Fatalf("全黑应判欠曝: %+v", e)
	}
	// 过曝：整帧糊到死白
	e = detectExposure(makeGray(q, func(x, y int) byte { return 255 }), q)
	if !e.Over || e.Under {
		t.Fatalf("全白应判过曝: %+v", e)
	}
	// 正常曝光：中间调且有层次，两个方向都不该命中
	e = detectExposure(makeGray(q, func(x, y int) byte { return byte(90 + (x*2+y)%60) }), q)
	if e.Under || e.Over {
		t.Fatalf("正常曝光不应命中: %+v", e)
	}
	// 白底文档（大部分近白但不是全白）——按校准结论不应判过曝
	e = detectExposure(makeGray(q, func(x, y int) byte {
		if (x/8+y/8)%2 == 0 {
			return 250 // 白纸
		}
		return 60 // 字迹
	}), q)
	if e.Over {
		t.Fatalf("白底文档不应判过曝: %+v", e)
	}
}

func TestDetectBlur(t *testing.T) {
	n := 256
	// 锐利：棋盘格，边缘密集 → 拉普拉斯响应大
	sharp := makeGray(n, func(x, y int) byte {
		if (x/4+y/4)%2 == 0 {
			return 30
		}
		return 220
	})
	// 模糊：左右平滑渐变，没有陡边 → 拉普拉斯响应很小
	blurry := makeGray(n, func(x, y int) byte { return byte(x * 255 / n) })
	ls, lb := lapStdOf(sharp, n, n), lapStdOf(blurry, n, n)
	if lb >= ls {
		t.Fatalf("模糊图拉普拉斯应更小: sharp=%.2f blurry=%.2f", ls, lb)
	}
	if ls < BlurLapStdThresh {
		t.Fatalf("棋盘格不该判模糊: lap=%.2f（阈值 %.2f）", ls, BlurLapStdThresh)
	}
	if lb >= BlurLapStdThresh {
		t.Fatalf("平滑渐变应判模糊: lap=%.2f（阈值 %.2f）", lb, BlurLapStdThresh)
	}
}

func TestDetectNoise(t *testing.T) {
	n := 512
	// 干净：大片平坦 + 少量结构，平坦块标准差接近 0
	clean := makeGray(n, func(x, y int) byte {
		if y < n/2 {
			return 128
		}
		return 200
	})
	// 噪点：同一结构叠上 ±25 的随机抖动
	noisy := makeGray(n, func(x, y int) byte {
		base := byte(128)
		if y >= n/2 {
			base = 200
		}
		d := (x*1103515245+y*12345)%51 - 25 // 确定性伪随机，避免测试不稳定
		v := int(base) + d
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		return byte(v)
	})
	fc, fn := noiseFloorOf(clean, n, n), noiseFloorOf(noisy, n, n)
	if fc > NoiseFloorThresh {
		t.Fatalf("干净图不应判噪点: floor=%.2f（阈值 %.2f）", fc, NoiseFloorThresh)
	}
	if fn <= NoiseFloorThresh {
		t.Fatalf("加噪图应判噪点: floor=%.2f（阈值 %.2f）", fn, NoiseFloorThresh)
	}
	if fn <= fc {
		t.Fatalf("加噪图噪声底线应更高: clean=%.2f noisy=%.2f", fc, fn)
	}
}

func TestRunSkipsWithoutFFmpeg(t *testing.T) {
	s := NewQCService("", "", "")
	r, err := s.Run(context.Background(), "nope.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if r != nil {
		t.Fatal("无 ffmpeg 应返回 nil")
	}
}
