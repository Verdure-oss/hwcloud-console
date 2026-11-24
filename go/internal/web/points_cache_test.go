package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hwcloud/internal/fsutil"
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
	resetPointsCache := func() {
		pointsCacheMu.Lock()
		pointsCache = nil
		lastDiskRead = time.Time{}
		lastDiskMtime = time.Time{}
		pointsCacheMu.Unlock()
	}
	resetPointsCache()
	t.Cleanup(resetPointsCache)

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
	resetPointsCache()

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

// TestPointsCacheReloadOnExternalWrite 验证另一个进程更新文件后，
// 本进程下次读取能看到新值（mtime 检测）。
//
// 这是 2026-11-24 实测踩过的坑：面板进程只在启动时读一次盘，
// CLI 定时任务更新文件后面板还显示旧值 —— 推送 1080、面板 1000。
func TestPointsCacheReloadOnExternalWrite(t *testing.T) {
	dir := t.TempDir()
	pointsCachePathOverride = filepath.Join(dir, "points_cache.json")
	t.Cleanup(func() { pointsCachePathOverride = "" })
	resetPointsCache := func() {
		pointsCacheMu.Lock()
		pointsCache = nil
		lastDiskRead = time.Time{}
		lastDiskMtime = time.Time{}
		pointsCacheMu.Unlock()
	}
	resetPointsCache()
	t.Cleanup(resetPointsCache)

	// 进程 A：写入 acct-1 = 1000
	storePointsEntry("acct-1", 1000)
	if e, _ := cachedPointsEntry("acct-1"); e.Points != 1000 {
		t.Fatalf("initial write: got %v", e)
	}

	// 模拟另一个进程（CLI）：绕过本进程内存，直接改文件。
	// mtime 精度问题：WriteFileAtomic 很快，两个写之间 mtime 可能相同，
	// 所以这里睡到 mtime 变化（最长 1.1 秒）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		other := map[string]pointsEntry{
			"acct-1": {Points: 1080, At: time.Now()},
			"acct-2": {Points: 730, At: time.Now()},
		}
		data, _ := json.MarshalIndent(other, "", "  ")
		if err := fsutil.WriteFileAtomic(pointsCachePathOverride, data, 0o600); err == nil {
			st, _ := os.Stat(pointsCachePathOverride)
			if !st.ModTime().Equal(lastDiskMtime) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("could not change file mtime in 2s")
		}
	}

	// 进程 A 再读：必须看到新值，不能卡在内存里的 1000。
	e1, ok1 := cachedPointsEntry("acct-1")
	if !ok1 || e1.Points != 1080 {
		t.Fatalf("external update not visible for acct-1: got %v ok=%v", e1, ok1)
	}
	e2, ok2 := cachedPointsEntry("acct-2")
	if !ok2 || e2.Points != 730 {
		t.Fatalf("external update not visible for acct-2: got %v ok=%v", e2, ok2)
	}
}
