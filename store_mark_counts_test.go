package main

import "testing"

// 回归（2026-11-02）：两个口径必须分开，别互相污染。
//
//   - FolderMarkCounts："有 review 行的文件数"（**含 decision=0 的清除遗留行**），
//     给「清空本目录标记」的确认弹窗做预览 —— 它算的是"会被抹掉的行数"。
//   - FolderDoneCount："已完成审阅"（decision 1/2），给目录进度条。
//     修复前进度条错用了 marked 口径：标了又清（decision=0）仍被算进已审，条永远满。
func TestFolderProgressCountsSemantics(t *testing.T) {
	s := newTestStore(t)
	fid := testFolderID(t, s)

	var ids []int64
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		res, err := s.db.Exec(`INSERT INTO file(folder_id, name, sort_key, rel_path, ext, size, mtime, kind, present)
			VALUES(?,?,?,?,?,1,1,1,1)`, fid, name, name, name, "jpg")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}

	// a=保留(alice) / b=不保留(bob) / c=保留后又清除(alice)
	for _, step := range []struct {
		id   int64
		dec  int
		user string
	}{{ids[0], 1, "alice"}, {ids[1], 2, "bob"}, {ids[2], 1, "alice"}, {ids[2], 0, "alice"}} {
		if _, err := s.SetDecision(step.id, step.dec, step.user); err != nil {
			t.Fatal(err)
		}
	}

	// 进度条口径：只有 a、b 算已审 → 2
	done, err := s.FolderDoneCount(fid)
	if err != nil {
		t.Fatal(err)
	}
	if done != 2 {
		t.Fatalf("FolderDoneCount 应为 2（decision=0 不算），实际 %d", done)
	}

	// 清空预览口径：3 行都会被抹掉 → 3（含 decision=0 的 c）
	rows, mine, err := s.FolderMarkCounts(fid, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 || mine != 2 {
		t.Fatalf("FolderMarkCounts 应为 3/2（含清除行），实际 %d/%d", rows, mine)
	}

	// 全清后两个口径都归零
	if _, err := s.ClearFolderReview(fid); err != nil {
		t.Fatal(err)
	}
	done, _ = s.FolderDoneCount(fid)
	rows, mine, _ = s.FolderMarkCounts(fid, "alice")
	if done != 0 || rows != 0 || mine != 0 {
		t.Fatalf("清空后应为 0/0/0，实际 done=%d rows=%d mine=%d", done, rows, mine)
	}
}
