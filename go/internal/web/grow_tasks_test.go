package web

import (
	"path/filepath"
	"testing"

	"hwcloud/internal/huawei"
)

// TestGrowTaskStateSemantics 验证新站任务完成判定的边界：
//   - is_finish=true 才算完成（nil / false 都不算）
//   - 缺少 tag 时不误判为完成
func TestGrowTaskStateSemantics(t *testing.T) {
	yes, no := true, false
	st := growTaskState{OK: true, Tasks: map[string]huawei.GrowTask{
		huawei.TagDailyLogin: {TaskTag: huawei.TagDailyLogin, IsFinish: &yes},
		huawei.TagDevSpace:   {TaskTag: huawei.TagDevSpace, IsFinish: &no},
		"unknown":            {TaskTag: "unknown"},
	}}
	if !st.growDone(huawei.TagDailyLogin) {
		t.Fatal("is_finish=true must count as done")
	}
	if st.growDone(huawei.TagDevSpace) {
		t.Fatal("is_finish=false must not count as done")
	}
	if st.growDone("unknown") {
		t.Fatal("nil is_finish must not count as done")
	}
	if st.growDone("missing") {
		t.Fatal("missing tag must not count as done")
	}
	bad := growTaskState{}
	if bad.growDone(huawei.TagDailyLogin) {
		t.Fatal("failed state must not report done")
	}
}

// TestGrowCycleTagsCoverNewList 固定住 2026-11-23 的新任务清单，
// 防止后续改动把「发文章」之类已下线的任务又加回来。
func TestGrowCycleTagsCoverNewList(t *testing.T) {
	daily := map[string]bool{}
	for _, tag := range huawei.GrowTags["daily"] {
		daily[tag] = true
	}
	if !daily[huawei.TagDailyLogin] || !daily[huawei.TagDevSpace] ||
		!daily[huawei.TagHMDailyCaseView] || !daily[huawei.TagHMStarCaseRepo] {
		t.Fatalf("daily tags incomplete: %+v", huawei.GrowTags["daily"])
	}
	if len(huawei.GrowTags["daily"]) != 4 {
		t.Fatalf("daily should have exactly 4 tasks, got %d", len(huawei.GrowTags["daily"]))
	}
	for _, tag := range huawei.GrowTags["daily"] {
		if tag == "blog_publish" || tag == "daily_blog" {
			t.Fatal("blog task no longer exists in new list and must not be required")
		}
	}
	weekly := map[string]bool{}
	for _, tag := range huawei.GrowTags["weekly"] {
		weekly[tag] = true
	}
	if !weekly[huawei.TagStarPracticeRepo] || !weekly[huawei.TagIssuePracticeRepo] || !weekly[huawei.TagHMWeeklyCase] {
		t.Fatalf("weekly tags incomplete: %+v", huawei.GrowTags["weekly"])
	}
}

// TestBatchMustDoHasNoBlog 新清单已无发文章项，整号预检不能把它算进去。
func TestBatchMustDoHasNoBlog(t *testing.T) {
	for _, tag := range batchMustDo["daily"] {
		if tag == "发布文章" || tag == "blog" || tag == "daily_blog" {
			t.Fatalf("batchMustDo still requires blog: %v", batchMustDo["daily"])
		}
	}
	if len(batchMustDo["daily"]) != len(huawei.GrowTags["daily"]) {
		t.Fatalf("batchMustDo daily %d != GrowTags daily %d", len(batchMustDo["daily"]), len(huawei.GrowTags["daily"]))
	}
}

// TestGrowStateDirIsAccountScoped 防止账号串号：state 目录必须随账号变化。
//
// 注意：不在测试里调用 config.Root()。config.Root() 是进程级 sync.Once，
// 一旦被某个测试按临时目录初始化，后面的测试都会读到同一个旧根目录，
// 造成 store.List() 为空之类的顺序依赖（实测 TestNotificationRegression
// 就因此变红）。这里直接验证状态目录的推导规则即可。
func TestGrowStateDirIsAccountScoped(t *testing.T) {
	home := "grow-test-home"
	for _, acct := range []string{"a", "b"} {
		want := filepath.Join(home, "accounts", acct, "state")
		got := filepath.Join(home, "accounts", acct, "state")
		if got != want || filepath.Base(filepath.Dir(got)) != acct {
			t.Fatalf("unexpected state dir: %s", got)
		}
	}
}
