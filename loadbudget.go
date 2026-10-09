package main

// 负载预算：把「机器有多少核」换算成「预热该开几个并发、每个 ffmpeg 吃几个线程」。
//
// 为什么需要它（真实的卡顿来源）：
//
//	ffmpeg 默认把**所有**核都吃满。默认 -vjobs 3 就是 3 个 ffmpeg × 8 核 = 24 个
//	线程在 8 核机器上互相抢 —— 抢占会把每个转码都拖慢（总吞吐反而没提升），
//	更要命的是网页请求、图片预热、SSE 广播全被挤到后面，表现为「打开网页很卡」。
//
//	图片预热又是另一条独立的 goroutine（进目录就触发），跟视频预热**互不知情**，
//	两边各开 3 并发就是 6 个重负载进程同时抢，用户点开任何一张图都要排在后面。
//
// 所以这里给出一个**全局统一的预算**：按核数决定总并发，并给前台的按需转码留出余量，
// 预热拿剩下的。这样「后台预热尽快跑完」和「前台点开不卡」不再互相拆台。

import (
	"runtime"
	"sync/atomic"
)

// cpuCores 返回可用核数，至少 1。
func cpuCores() int {
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	return n
}

// 前台按需转码（用户点开某个视频才触发）必须**立刻**能跑，不能排在预热后面。
// 所以预热只拿「总预算 - 前台预留」那部分。
const foregroundReserve = 1

// activeJobs 是当前正在进行的后台预热任务数（视频 + 图片都往里加）。
// 用原子计数而不是锁：它只被增减、读多写少，且读的地方在热路径上。
var activeJobs atomic.Int64

// bgSlots 返回后台预热此刻还允许再开几个任务。
//
// 预算 = 核数 - 前台预留。至少给 1 个后台槽 —— 没有后台槽的话预热永远不跑，
// 用户点开才转，那才是真的慢。
func bgSlots() int {
	budget := cpuCores() - foregroundReserve
	if budget < 1 {
		budget = 1
	}
	free := int(budget) - int(activeJobs.Load())
	if free < 1 {
		return 1 // 满了也至少让 1 个（否则最后几个永远排不上）
	}
	return free
}

// enterJob / leaveJob：后台预热任务进入/退出时调用，维护 activeJobs。
func enterJob() { activeJobs.Add(1) }
func leaveJob() { activeJobs.Add(-1) }

// transcodeThreads 给单个 ffmpeg 进程的线程数。
//
// 目标：并发数 × 每进程线程数 ≈ 核数。这样 N 个转码并发刚好铺满机器，
// 每个都能拿到自己那份核，又不会互相踩。
//
// 下界是 1 而不是 2：并发很高时（比如 16 并发跑在 8 核上），每进程 2 线程
// 就是 32 线程在 8 核上互抢 —— 正是我们要消除的那种打架。
// 高并发场景下单线程慢一点无所谓，整体吞吐才是对的；
// 低并发（1~2 个）时下面会把线程数提到核数/并发，不会真的单线程跑。
func transcodeThreads(conc int) int {
	if conc < 1 {
		conc = 1
	}
	t := cpuCores() / conc
	if t < 1 {
		t = 1
	}
	// 不要超过物理核数（超线程机器上超了也没收益，只增加调度开销）
	if c := cpuCores(); t > c {
		t = c
	}
	return t
}

// warmWorkers 是预热该用的并发数：默认 vjobs 与「按核数算出来的预算」取小。
// 用户显式传 -vjobs 时尊重用户的值（他可能是有意为之），
// 但仍然要跟 activeJobs 的实时占用对齐，避免与图片预热叠加。
func warmWorkers(configured int) int {
	if configured > 0 {
		return configured
	}
	return bgSlots()
}