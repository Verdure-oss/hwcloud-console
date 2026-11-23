package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPointsCachePersistence 验证积分缓存的跨进程持久化：
// 进程 A 写入 → 进程 B（新的 loadPointsCacheLocked）能读到。
//
// 这正是 CLI 定时任务与面板进程共享积分的场景：
// CLI 跑完任务写入，面板下次轮询直接显示缓存值，不再现查。
func TestPointsCachePersistence(t *testing.T) {
	// 注意：这里**不能**让 pointsCachePath() 走 config.Root()。
	// Root() 是 sync.Once，第一个碰到它的测试会把项目根目录钉死在
	// 整个测试进程里，后面的测试（即使 t.Setenv 换了 HWCLOUD_HOME）
	// 全部读错目录 —— TestNotificationRegression 就是这样被搞挂的。
	// 用 override 把缓存文件指向本测试自己的 TempDir。
	dir := t.TempDir()
	pointsCachePathOverride = filepath.Join(dir, "points_cache.json")
	t.Cleanup(func() { pointsCachePathOverride = "" })

	// 重置进程内缓存状态（模拟新进程）。
	pointsCacheMu.Lock()
	pointsCache = nil
	pointsLoaded = false
	pointsCacheMu.Unlock()
	t.Cleanup(func() {
		pointsCacheMu.Lock()
		pointsCache = nil
		pointsLoaded = false
		pointsCacheMu.Unlock()
	})

	storePointsEntry("acct-1", 1000)
	storePointsEntry("acct-2", 640)
	// batchAllID 与非法值必须被拒。
	storePointsEntry("all", 99999)
	storePointsEntry("acct-3", -5)

	// 进程内立即可读。
	if e, ok := cachedPointsEntry("acct-1"); !ok || e.Points != 1000 {
		t.Fatalf("in-process read: got %v ok=%v", e, ok)
	}

	// 模拟另一个进程：清空内存状态后重读磁盘。
	pointsCacheMu.Lock()
	pointsCache = nil
	pointsLoaded = false
	pointsCacheMu.Unlock()

	e2, ok2 := cachedPointsEntry("acct-2")
	if !ok2 || e2.Points != 640 {
		t.Fatalf("cross-process read: got %v ok=%v", e2, ok2)
	}
	if _, ok := cachedPointsEntry("all"); ok {
		t.Fatal("batchAllID must not be cached")
	}
	if _, ok := cachedPointsEntry("acct-3"); ok {
		t.Fatal("negative points must not be cached")
	}
	if _, ok := cachedPointsEntry(""); ok {
		t.Fatal("empty id must not be cached")
	}
	if _, ok := cachedPointsEntry("missing"); ok {
		t.Fatal("unknown id must miss")
	}

	// 时间戳必须被记录（前端/排查要知道缓存新鲜度）。
	if e2.At.IsZero() || time.Since(e2.At) > time.Minute {
		t.Fatalf("timestamp missing or stale: %v", e2.At)
	}

	// 缓存文件落在 override 指定的位置。
	if _, err := os.Stat(pointsCachePathOverride); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
}
