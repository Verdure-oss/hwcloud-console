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
	// lastDiskRead 记录上次从磁盘加载的时间。
	//
	// 【为什么不能只在进程启动时读一次】实测踩过（2026-11-24）：
	// 缓存文件是跨进程共享的 —— 面板进程 20:48 读过一次盘后 pointsLoaded
	// 恒为 true，第二天早上 CLI 定时任务把文件更新成 1080 分，
	// 面板却还显示内存里的 1000 分，用户看到推送 1080、面板 1000。
	// 现在按 mtime 判断：文件变了就重读，文件没变就读内存（零成本）。
	lastDiskRead  time.Time
	lastDiskMtime time.Time
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
//
// 每次读操作都会走到这里，但不会每次都真读盘：
// 先 stat 文件 mtime，和上次加载时一样就直接用内存（列表 45 秒轮询
// 全走内存，stat 一次的开销可以忽略）；mtime 变了才重读。
// 这样 CLI 进程更新文件后面板下次轮询立刻能看到新值，
// 同时避免每次轮询都做无谓的文件读取。
//
// 文件不存在/损坏都按空缓存处理 —— 缓存丢了只是多一次显式查询，不是错误。
func loadPointsCacheLocked() {
	// 内存缓存必须保证非 nil。
	if pointsCache == nil {
		pointsCache = map[string]pointsEntry{}
	}
	path := pointsCachePath()
	st, err := os.Stat(path)
	if err != nil {
		// 文件不存在：清空内存缓存（比如测试隔离、文件被删）。
		// 只在「上次加载成功过」后才清 —— 首次调用时内存本来就是空的。
		if !lastDiskRead.IsZero() {
			pointsCache = map[string]pointsEntry{}
			lastDiskRead = time.Now()
			lastDiskMtime = time.Time{}
		}
		return
	}
	if !lastDiskRead.IsZero() && st.ModTime().Equal(lastDiskMtime) {
		return // 文件没变，内存即最新
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	fresh := map[string]pointsEntry{}
	if err := json.Unmarshal(data, &fresh); err != nil {
		return // 损坏的文件不覆盖内存里的有效值
	}
	pointsCache = fresh
	lastDiskRead = time.Now()
	lastDiskMtime = st.ModTime()
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
//
// 注意写路径直接更新内存并记录 mtime —— 不经过 loadPointsCacheLocked
// 的「重读」分支，避免自己刚写的文件被当作「外部变更」再读一遍。
// 但在写之前要先同步一次磁盘状态：如果 CLI 进程刚更新过文件，
// 内存里还是旧值，直接覆盖写会把别的账号的新积分抹掉。
// stat 一次拿到最新 mtime → 需要时重读 → 再合并写入。
// 落盘失败只记日志 —— 缓存写不进去无非是下次重启后显示旧值，不影响业务。
func storePointsEntry(id string, n int) {
	if id == "" || id == batchAllID || n < 0 {
		return
	}
	pointsCacheMu.Lock()
	// 内存缓存必须先保证非 nil（首次写入时文件还不存在，
	// 下面的 stat 分支不会初始化它，直接赋值会 panic nil map）。
	if pointsCache == nil {
		pointsCache = map[string]pointsEntry{}
	}
	// 再吸收磁盘上的最新状态（别的进程可能刚写过）
	if st, err := os.Stat(pointsCachePath()); err == nil {
		if lastDiskRead.IsZero() || !st.ModTime().Equal(lastDiskMtime) {
			if data, err := os.ReadFile(pointsCachePath()); err == nil {
				fresh := map[string]pointsEntry{}
				if json.Unmarshal(data, &fresh) == nil {
					pointsCache = fresh
					pointsCache[id] = pointsEntry{Points: n, At: time.Now()}
				}
			}
			lastDiskRead = time.Now()
			lastDiskMtime = st.ModTime()
		}
	}
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
