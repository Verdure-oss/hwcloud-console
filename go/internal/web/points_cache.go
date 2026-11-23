package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/fsutil"
	"hwcloud/internal/huawei"
)

// 积分缓存（跨进程持久化）。
//
// 需求（2026-11-23）：面板上的积分显示**缓存值**，不要每次轮询都去
// 远程接口现查 —— 现查要走 cookie 调远程接口，会话稍有不稳就会触发
// 一整条重登链路，列表页也跟着变慢。积分本身变化很慢（只有做任务才变），
// 所以缓存的更新时机是「做完任务 / 用户显式操作」：
//
//   - 做完任务：handleRunTask 同步任务后、finishJob 异步任务后、
//     RunBatchForCLI 每个账号后、签到结果自带总分
//   - 显式操作：点「积分」按钮（name=beans）、点「检查」按钮
//   - 推送路径：beansForNotify 现查成功后顺带落盘
//
// 为什么落盘而不是只放内存：定时任务（hwcloud-daily.sh）走 CLI，
// 是**独立进程** —— CLI 跑完任务把新积分写进这份文件，面板进程
// （以及面板重启后）读到的就是新值，两个进程共享同一份缓存。

// pointsEntry 是单个账号的积分缓存项。
type pointsEntry struct {
	Points int       `json:"points"`
	At     time.Time `json:"at"`
}

var (
	pointsCacheMu sync.Mutex
	pointsCache   map[string]pointsEntry
	pointsLoaded  bool
)

// pointsCachePathOverride 仅供测试使用：测试不能碰 config.Root()，
// 因为它是 sync.Once —— 第一个测试会把项目根目录钉死在整个测试进程里，
// 后面的测试（即使 t.Setenv 换了 HWCLOUD_HOME）都读不到自己的目录。
var pointsCachePathOverride string

// pointsCachePath 返回积分缓存文件路径（所有账号共用一份，按 id 分键）。
func pointsCachePath() string {
	if pointsCachePathOverride != "" {
		return pointsCachePathOverride
	}
	return filepath.Join(config.Root(), "state", "points_cache.json")
}

// loadPointsCacheLocked 从磁盘加载缓存（调用方必须已持锁）。
// 文件不存在/损坏都按空缓存处理 —— 缓存丢了只是多一次显式查询，不是错误。
func loadPointsCacheLocked() {
	if pointsLoaded {
		return
	}
	pointsLoaded = true
	pointsCache = map[string]pointsEntry{}
	data, err := os.ReadFile(pointsCachePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &pointsCache)
}

// cachedPointsEntry 读某账号的缓存积分。只读，不发任何网络请求、不碰锁。
func cachedPointsEntry(id string) (pointsEntry, bool) {
	if id == "" {
		return pointsEntry{}, false
	}
	pointsCacheMu.Lock()
	defer pointsCacheMu.Unlock()
	loadPointsCacheLocked()
	e, ok := pointsCache[id]
	return e, ok
}

// storePointsEntry 写缓存：先更内存，再原子落盘。
// 落盘失败只记日志 —— 缓存写不进去无非是下次重启后显示旧值，不影响业务。
func storePointsEntry(id string, n int) {
	if id == "" || id == batchAllID || n < 0 {
		return
	}
	pointsCacheMu.Lock()
	loadPointsCacheLocked()
	pointsCache[id] = pointsEntry{Points: n, At: time.Now()}
	data, err := json.MarshalIndent(pointsCache, "", "  ")
	pointsCacheMu.Unlock()
	if err != nil {
		return
	}
	if err := fsutil.WriteFileAtomic(pointsCachePath(), data, 0o600); err != nil {
		svcLog("[points] 积分缓存落盘失败: %v", err)
	}
}

// refreshAccountBeans 现查某账号的新站积分并写缓存。
//
// 安全性：用 huawei.NewGrowSessionForAccount 直接读该账号目录的 cookie，
// 不碰进程级全局 accountID、不拿任何锁 —— 可以在任意上下文调用。
// 只调只读接口（GET /user/info），会话失效时如实返回 false，
// **绝不触发登录**；要不要重登由用户显式操作决定。
func refreshAccountBeans(id string) (int, bool) {
	dir := filepath.Join(config.Root(), "accounts", id, "state")
	if id == "" {
		dir = filepath.Join(config.Root(), "state")
	}
	gs, err := huawei.NewGrowSessionForAccount(dir)
	if err != nil {
		return 0, false
	}
	n, err := gs.Points()
	if err != nil {
		return 0, false
	}
	storePointsEntry(id, n)
	return n, true
}

// refreshBeansAfterJob 异步任务结束后刷新积分缓存。
//
// 单账号任务只刷该账号；「全部账号」批量任务按结果里的账号清单逐个刷。
// 放在 goroutine 里调用（见 finishJob），不阻塞 job 收尾和 /api/jobs 轮询。
func (s *Server) refreshBeansAfterJob(n *jobNotify) {
	if n == nil {
		return
	}
	if n.Account != batchAllID {
		refreshAccountBeans(n.Account)
		return
	}
	ids := map[string]bool{}
	if raw, err := json.Marshal(n.Result["accounts"]); err == nil {
		var accounts []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &accounts) == nil {
			for _, a := range accounts {
				ids[a.ID] = true
			}
		}
	}
	if len(ids) == 0 {
		for _, a := range s.store.List() {
			ids[a.ID] = true
		}
	}
	for id := range ids {
		refreshAccountBeans(id)
	}
}
