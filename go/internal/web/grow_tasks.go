package web

import (
	"fmt"
	"strings"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
)

// 新版成长中心（grow.developer.huaweicloud.com）的签到与任务状态。
//
// 旧 runDaily 的顶部预检、签到、周常 weeklyMissionDone 全部走
// devdata 那套接口，2026-11-23 已确认下线。这里用新站 /user/tasks +
// /user/sign-in 作为权威状态源，旧接口只作为兜底。

// growTaskState 是某个账号在新站的任务完成快照。
type growTaskState struct {
	OK    bool // 是否成功取到状态
	Err   string
	Tasks map[string]huawei.GrowTask // tag -> task
}

// loadGrowState 取当前账号的新站任务状态（在 withAccount 内调用）。
func loadGrowState() growTaskState {
	st := growTaskState{Tasks: map[string]huawei.GrowTask{}}
	sess, err := huawei.NewGrowSession()
	if err != nil {
		st.Err = err.Error()
		return st
	}
	tasks, err := sess.Tasks()
	if err != nil {
		st.Err = err.Error()
		return st
	}
	for _, t := range tasks {
		st.Tasks[t.TaskTag] = t
	}
	st.OK = true
	return st
}

// growDone 判断某个任务 tag 在当前周期是否已完成。
func (st growTaskState) growDone(tag string) bool {
	t, ok := st.Tasks[tag]
	return ok && t.Finished()
}

// growSign 在新站签到（幂等：已签直接返回）。
func (s *Server) growSign() (map[string]any, error) {
	sess, err := huawei.NewGrowSession()
	if err != nil {
		return nil, err
	}
	u, err := sess.UserInfo()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"total_points": u.TotalPoints,
	}
	// user/info 自带总分，顺手写持久缓存（免费更新时机）。
	if u.TotalPoints > 0 {
		storePointsEntry(config.AccountID(), u.TotalPoints)
	}
	// 签到状态以任务清单里的 daily_login 为准。
	// /user/info 没有 signed 字段（实测 keys 里不含），拿它判断恒为未签到，
	// 会导致每次点签到都去撞一次接口、页面也永远显示「未签到」。
	if done, serr := sess.SignedToday(); serr == nil {
		out["signed"] = done
		if done {
			out["already"] = true
			out["msg"] = "今日已签到"
			return out, nil
		}
	}
	r, err := sess.SignIn()
	if err != nil {
		return nil, err
	}
	out["result"] = r
	out["signed"] = r.Signed
	out["total_points"] = r.TotalPoints
	// 2026-11-23：签到响应自带最新总分，免费拿到 —— 顺手写持久缓存。
	if r.TotalPoints > 0 {
		storePointsEntry(config.AccountID(), r.TotalPoints)
	}
	if r.Signed {
		out["msg"] = fmt.Sprintf("签到成功 +%d", r.Points)
	} else {
		out["msg"] = "签到未生效"
	}
	return out, nil
}

// growPoints 返回当前账号新站积分（失败返回 ok=false）。
func (s *Server) growPoints() (int, bool) {
	sess, err := huawei.NewGrowSession()
	if err != nil {
		return 0, false
	}
	n, err := sess.Points()
	if err != nil {
		return 0, false
	}
	storePointsEntry(config.AccountID(), n)
	return n, true
}

// growTagsDone 返回指定周期内已完成的 tag 集合（用于整号预检）。
func growTagsDone(cycle string) (map[string]bool, bool) {
	st := loadGrowState()
	if !st.OK {
		return nil, false
	}
	done := map[string]bool{}
	for _, tag := range huawei.GrowTags[cycle] {
		if st.growDone(tag) {
			done[tag] = true
		}
	}
	return done, true
}

// growPendingTitles 把未完成的 tag 转成中文标题，便于写进步骤/通知。
func growPendingTitles(cycle string) []string {
	done, ok := growTagsDone(cycle)
	if !ok {
		return nil
	}
	var out []string
	for _, tag := range huawei.GrowTags[cycle] {
		if !done[tag] {
			out = append(out, huawei.GrowTagTitle(tag))
		}
	}
	return out
}

// growAllDone 判断某周期任务是否全部完成。查询失败时返回 false（宁可多跑）。
func growAllDone(cycle string) bool {
	done, ok := growTagsDone(cycle)
	if !ok {
		return false
	}
	for _, tag := range huawei.GrowTags[cycle] {
		if !done[tag] {
			return false
		}
	}
	return len(huawei.GrowTags[cycle]) > 0
}

// growDailyReport 把新站 daily 任务状态写进 runDaily 的步骤里。
func (s *Server) growDailyReport(done map[string]bool) []map[string]any {
	var steps []map[string]any
	for _, tag := range huawei.GrowTags["daily"] {
		name := huawei.GrowTagTitle(tag)
		if done[tag] {
			steps = append(steps, map[string]any{"name": name, "ok": true, "msg": "今日已完成"})
		} else {
			steps = append(steps, map[string]any{"name": name, "ok": false, "msg": "未完成"})
		}
	}
	return steps
}

// growEnsureAccount 确保账号的新站会话可用于当前账号（供面板/定时共用）。
//
// 只做只读探活：不触发登录，不重跑任务。
func growEnsureAccount() error {
	sess, err := huawei.NewGrowSession()
	if err != nil {
		return err
	}
	if _, err := sess.UserInfo(); err != nil {
		return fmt.Errorf("新成长中心会话不可用：%w", err)
	}
	return nil
}

// growCycleLabel 把 cycle 转成中文。
func growCycleLabel(cycle string) string {
	switch strings.ToLower(cycle) {
	case "daily":
		return "日常"
	case "weekly":
		return "周常"
	case "monthly":
		return "月度"
	case "limit":
		return "一次性"
	}
	return cycle
}

// hasGrowTaskLike 判断新站任务清单里是否有 tag/标题命中关键词的任务。
//
// 用途：旧清单里的「开发者活动」「开发者赛事」在新版已不存在，
// 但代码还保留着执行能力作兼容。先查新站是否有对应项，
// 没有就跳过，避免对着已下线的入口做无用功。
//
// excludeTags 用于排除「看起来像但不是同一件事」的新任务：
// 例如「报名华为ICT大赛创新赛」标题含「大赛」，但它走的是独立的
// ICT 报名链路（步骤 7），不能被旧「开发者赛事」流程误当成同一项。
func hasGrowTaskLike(st growTaskState, excludeTags []string, keywords ...string) bool {
	if !st.OK {
		// 状态读不到时保守执行，避免漏做。
		return true
	}
	excluded := map[string]bool{}
	for _, t := range excludeTags {
		excluded[t] = true
	}
	for tag, t := range st.Tasks {
		if excluded[tag] {
			continue
		}
		low := strings.ToLower(tag)
		for _, kw := range keywords {
			if strings.Contains(low, strings.ToLower(kw)) || strings.Contains(t.Title, kw) {
				return true
			}
		}
	}
	return false
}

// cycleDegree 把新站周期映射成面板原来的「星级」展示（兼容旧前端）。
// 新平台没有星级概念，这里按积分档位给个近似值，仅用于排序/展示。
func cycleDegree(cycle string) int {
	switch strings.ToUpper(cycle) {
	case "DAILY":
		return 1
	case "WEEKLY":
		return 2
	case "MONTHLY":
		return 3
	case "LIMIT":
		return 4
	}
	return 1
}

// growStateDir 返回当前账号的 state 目录（给日志/调试用）。
func growStateDir() string {
	return config.StateDir()
}

var _ = time.Now
