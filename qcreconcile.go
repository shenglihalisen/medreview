package main

// 质量检测的「跨照片对账」：单张判定给出的结论，用同设备的多张照片互相印证。
//
// 两条领域规则（2026-09-26 定稿）：
//  1. 连拍近似重复 —— 同一秒 + 同一型号的照片是连拍，帧与帧之间 pHash 不会完全
//     相同但很接近（全库"精确相同"的比对抓不到）；组内以最小 file_id 为锚，
//     其余与锚汉明距离 ≤ 阈值的都标重复（锚 = 留下的那张）。
//  2. 镜头污损确认 —— 污点固定在镜头上，同一设备的多张照片会在画面**同一位置**
//     出现同样的脏点框；单张独有的大概率是场景内容（过曝的天空、灯光、白墙），
//     直接清掉。同设备可用照片不足时不做确认（没有参照物，维持单张判定）。
//
// 关键设计：reconcilePlan 是**纯函数**（行进、改动出，不碰库不碰网），
// 方便单测；落库和广播由调用方做。

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"medreview/qc"
)

// reconcilePlan 计算需要改动的文件及其新 flags。
// 返回 map[fileID]新flags —— 调用方对比旧值，变了才落库广播。
func reconcilePlan(rows []QCRow) (map[int64]int, [][2]int64) {
	// dupCandidates：连拍簇内的 hash 候选对 [锚id, 成员id]。
	// 调用方（QCManager）必须做像素确认后才落库 —— hash 看不出"背景同、主体不同"。
	dupCandidates := [][2]int64{}
	type rt struct {
		id      int64
		flags   int
		hash    uint64
		hasHash bool
		boxes   []qc.Box
		device  string
		shoot   string
		ts      time.Time
	}
	all := make([]rt, 0, len(rows))
	for _, r := range rows {
		var it rt
		it.id, it.flags, it.device, it.shoot = r.FileID, r.Flags, r.Device, r.ShootKey
		if r.Flags&qc.FlagCorrupted != 0 {
			it.flags |= 0 // 损坏件：hash 无意义，不参与任何分组比对
		} else if r.DupHash != "" {
			if _, e := fmt.Sscanf(r.DupHash, "%016x", &it.hash); e == nil {
				it.hasHash = true
			}
		}
		if r.Detail != "" {
			var res qc.QCResult
			if json.Unmarshal([]byte(r.Detail), &res) == nil && res.LensDirt != nil {
				it.boxes = res.LensDirt.Boxes
			}
		}
		all = append(all, it)
	}

	// add/clear 分开记：混在同一个 int 里没法区分「要加的位」和「要清的位」
	// （| 完之后清位的痕迹就没了）。
	type delta struct{ add, clear int }
	changes := map[int64]*delta{}
	idx := map[int64]*rt{}
	for i := range all {
		idx[all[i].id] = &all[i]
	}
	mark := func(id int64, f func(d *delta)) {
		d := changes[id]
		if d == nil {
			d = &delta{}
			changes[id] = d
		}
		f(d)
	}

	// 按设备分组（对账的两个规则都只对「同设备」成立）
	byDev := map[string][]*rt{}
	for i := range all {
		it := &all[i]
		if it.device != "" && it.flags&qc.FlagCorrupted == 0 {
			byDev[it.device] = append(byDev[it.device], it)
		}
	}

	// ---- 1) 连拍组近似重复 ----
	// 同设备 + 拍摄时间相邻（间隔 ≤ 窗口）的照片聚成一簇；连拍会跨秒，
	// 拿「同一秒」当分组键会拆散一组。簇内以最小 file_id 为锚，其余与锚
	// 汉明 ≤ 阈值的标重复。没有拍摄时间的（EXIF 和文件名都没有）不参与。
	for _, group := range byDev {
		var cand []*rt
		for _, it := range group {
			if it.hasHash && it.shoot != "" {
				if ts, e := time.Parse("2006-01-02 15:04:05", it.shoot); e == nil {
					it.ts = ts
					cand = append(cand, it)
				}
			}
		}
		sort.Slice(cand, func(a, b int) bool { return cand[a].ts.Before(cand[b].ts) })
		var cluster []*rt
		flush := func() {
			defer func() { cluster = nil }()
			if len(cluster) < 2 {
				return
			}
			anchor := cluster[0]
			for _, it := range cluster {
				if it.id < anchor.id {
					anchor = it
				}
			}
			for _, it := range cluster {
				if it.id == anchor.id {
					continue
				}
				if qc.Hamming(anchor.hash, it.hash) <= qc.DupHammingThresh {
					dupCandidates = append(dupCandidates, [2]int64{anchor.id, it.id})
					mark(it.id, func(d *delta) { d.add |= qc.FlagDup })
				}
			}
		}
		for _, it := range cand {
			if len(cluster) > 0 && it.ts.Sub(cluster[len(cluster)-1].ts) > time.Duration(qc.BurstWindowSec)*time.Second {
				flush()
			}
			cluster = append(cluster, it)
		}
		flush()
	}

	// ---- 2) 镜头污损跨照片确认 ----
	for _, group := range byDev {
		if len(group) < qc.DirtConfirmMin {
			continue // 没有足够参照物：维持单张判定，不清
		}
		for _, it := range group {
			if it.flags&qc.FlagLensDirt == 0 || len(it.boxes) == 0 {
				continue
			}
			confirmed := false
			for _, other := range group {
				if other.id == it.id {
					continue
				}
				for _, ob := range other.boxes {
					for _, b := range it.boxes {
						if b.IoU(ob) >= qc.DirtIoUThresh {
							confirmed = true
							break
						}
					}
					if confirmed {
						break
					}
				}
				if confirmed {
					break
				}
			}
			if !confirmed {
				// 单张独有 → 大概率是场景内容，清掉脏污位（boxes 留在 detail 里无妨，
				// 前端只在 flags 命中时才画框）
				mark(it.id, func(d *delta) { d.clear |= qc.FlagLensDirt })
			}
		}
	}

	// 展开成最终 flags：先加后清
	out := map[int64]int{}
	for id, d := range changes {
		if it := idx[id]; it != nil {
			out[id] = (it.flags | d.add) &^ d.clear
		}
	}
	return out, dupCandidates
}
