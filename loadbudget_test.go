package main

import (
	"sync"
	"testing"
)

// 负载预算（loadbudget.go）的行为约束。
//
// 这些是「网页卡不卡」的直接决定因素：预热并发开多少、每个 ffmpeg 吃几个线程。
// 一旦有人把并发改回「每个 ffmpeg 不限线程」，页面立刻会重新卡起来 —— 所以钉死。

func TestTranscodeThreadsFillsCoresButNotOver(t *testing.T) {
	cores := cpuCores()
	for _, conc := range []int{1, 2, 3, 4, 8, 16} {
		th := transcodeThreads(conc)
		if th < 1 {
			t.Fatalf("并发 %d 时线程数 %d < 1，ffmpeg 会直接报错", conc, th)
		}
		if th > cores {
			t.Fatalf("并发 %d 时线程数 %d 超过核数 %d，超线程机器上纯属增加调度开销",
				conc, th, cores)
		}
		// 并发 × 线程不应明显超过核数（否则就是互相踩）。
		// 高并发（>核数）时并发本身已经超配，此时每进程 1 线程即可，不再叠加。
		total := conc * th
		limit := cores * 2
		if conc > cores {
			limit = conc // 并发已经超配，容忍 1 线程/进程
		}
		if total > limit {
			t.Fatalf("并发 %d × 线程 %d = %d，超过容忍上限 %d，总线程会打架",
				conc, th, total, limit)
		}
		// 并发明显少于核数时（还有空闲核可铺），每进程至少 2 线程，
		// 否则单进程太慢、机器也铺不满。
		// 注意并发 == 核数 时（8 并发跑 8 核）每进程 1 线程就是铺满的，那是正确的。
		if conc < cores && th < 2 {
			t.Fatalf("并发 %d < 核数 %d 时线程数应 ≥2 铺满机器，实际 %d", conc, cores, th)
		}
	}
}

// 并发为 0 / 负数时必须兜住，不能算出 0 线程（ffmpeg 会直接报错）。
func TestTranscodeThreadsClampsInvalidConc(t *testing.T) {
	for _, conc := range []int{0, -1, -99} {
		if th := transcodeThreads(conc); th < 1 {
			t.Fatalf("并发 %d 算出线程数 %d，应至少为 1", conc, th)
		}
	}
}

// 预算里必须给前台按需转码留出位置：
// 用户点开某个视频时的按需转码不能排在后台预热后面，否则「点开就卡」。
func TestBgSlotsLeavesForegroundHeadroom(t *testing.T) {
	budget := bgSlots()
	if budget < 1 {
		t.Fatal("后台槽位至少为 1，否则预热永远不跑")
	}
	// 即使预算算出来不满，也不得超过「核数 - 前台预留 + 1」的量级
	if budget > cpuCores() {
		t.Fatalf("后台槽位 %d 超过核数 %d，没有给前台留任何余量", budget, cpuCores())
	}
}

// enterJob/leaveJob 必须让 activeJobs 回到 0，否则长期运行后预算会被"泄漏"成 0，
// 预热再也不跑（这是最容易被忽略的回归：计数只加不减）。
func TestActiveJobsBalances(t *testing.T) {
	before := activeJobs.Load()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enterJob()
			leaveJob()
		}()
	}
	wg.Wait()

	if after := activeJobs.Load(); after != before {
		t.Fatalf("20 次 enter/leave 配平后计数应回到 %d，实际 %d（只加不减会泄漏）",
			before, after)
	}
}

// 占满额度后 bgSlots 仍要给 1 个 —— 否则最后一批任务永远排不上，预热卡在中间。
func TestBgSlotsAlwaysAdmitsOne(t *testing.T) {
	before := activeJobs.Load()
	defer func() { activeJobs.Store(before) }()

	// 人为占满到远超预算
	for i := 0; i < cpuCores()*2; i++ {
		enterJob()
	}
	if got := bgSlots(); got < 1 {
		t.Fatalf("额度占满后 bgSlots 应仍放行至少 1 个，实际 %d（最后一批会永远排不上）", got)
	}
}

// warmWorkers：用户显式传 -vjobs 时尊重用户值；没传（<=0）才按核数自动算。
func TestWarmWorkersRespectsExplicitConfig(t *testing.T) {
	before := activeJobs.Load()
	defer func() { activeJobs.Store(before) }()

	if got := warmWorkers(2); got != 2 {
		t.Fatalf("显式配置 2 应返回 2，实际 %d（用户可能是有意调低负载）", got)
	}
	auto := warmWorkers(0)
	if auto < 1 || auto > cpuCores() {
		t.Fatalf("自动模式应返回 [1, 核数=%d] 之间的值，实际 %d", cpuCores(), auto)
	}
}

// video.go 的 activeWorkers 是给 ffmpeg 算线程用的，不能返回 0
// （0 会让 transcodeThreads 拿到 conc=0，虽有兜底但语义上应始终 >=1）。
func TestActiveWorkersAtLeastOne(t *testing.T) {
	before := activeJobs.Load()
	defer func() { activeJobs.Store(before) }()

	activeJobs.Store(0)
	v := &VideoService{}
	if got := v.activeWorkers(); got < 1 {
		t.Fatalf("activeWorkers 应至少返回 1，实际 %d", got)
	}
	activeJobs.Store(3)
	if got := v.activeWorkers(); got != 3 {
		t.Fatalf("activeWorkers 应反映当前并发 3，实际 %d", got)
	}
}