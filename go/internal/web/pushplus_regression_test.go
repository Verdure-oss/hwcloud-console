package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hwcloud/internal/config"
)

type notifyMock func(*http.Request) (*http.Response, error)

type beanFixture struct {
	value int
	err   error
}

func (b beanFixture) BeansE() (int, error) { return b.value, b.err }
func (b beanFixture) Points() (int, error) { return b.value, b.err }

func (f notifyMock) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotificationRegression(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HWCLOUD_HOME", home)
	// 隔离持久积分缓存：beansForNotify 现查失败时会回读 points_cache.json，
	// 不清掉的话别的测试（或真实 HWCLOUD_HOME）留下的缓存会泄进断言。
	resetPointsCache := func() {
		pointsCacheMu.Lock()
		pointsCache = nil
		lastDiskRead = time.Time{}
		lastDiskMtime = time.Time{}
		pointsCacheMu.Unlock()
	}
	pointsCachePathOverride = filepath.Join(t.TempDir(), "points_cache.json")
	t.Cleanup(func() { pointsCachePathOverride = "" })
	resetPointsCache()
	t.Cleanup(resetPointsCache)
	for _, id := range []string{"fake-a", "fake-b"} {
		dir := filepath.Join(home, "accounts", id)
		os.MkdirAll(filepath.Join(dir, "state"), 0700)
		os.WriteFile(filepath.Join(dir, "account.json"), []byte(`{"id":"`+id+`"}`), 0600)
		// Invalid fixture prevents any real Huawei HTTP request.
		os.WriteFile(filepath.Join(dir, "state", "cookies.json"), []byte(`invalid-fixture`), 0600)
	}
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	s.probeCache["fake-a"] = probeSnapshot{Beans: 99999, At: time.Now()}
	cfg := defaultPushPlusConfig()
	cfg.Token = "FAKE_TEST_ONLY"
	pushCfg = cfg
	defer func() { pushCfg = nil }()
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	var messages []PushPlusPayload
	oldSession := newNotifySession
	defer func() { newNotifySession = oldSession }()
	oldGrowSession := newGrowNotifySession
	defer func() { newGrowNotifySession = oldGrowSession }()
	queried := map[string]int{}
	// 新站取积分是首选路径；测试里让它全部失败，从而回落到旧会话 fixture。
	newGrowNotifySession = func(dir string) (growNotifySession, error) {
		return beanFixture{err: fmt.Errorf("mock grow offline")}, nil
	}
	newNotifySession = func(dir string) (notifyBeanSession, error) {
		id := filepath.Base(filepath.Dir(dir))
		queried[id]++
		if id == "fake-a" {
			return beanFixture{value: 123}, nil
		}
		return beanFixture{err: fmt.Errorf("mock offline")}, nil
	}
	http.DefaultTransport = notifyMock(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.pushplus.plus" {
			t.Fatal("unexpected outbound request")
		}
		var p PushPlusPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, p)
		time.Sleep(25 * time.Millisecond)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":"mock-id"}`)), Header: make(http.Header)}, nil
	})
	for _, tc := range []struct {
		name       string
		result     map[string]any
		done, fail bool
		want       int
		status     string
	}{
		{"daily", map[string]any{"ok": true}, true, true, 1, "成功"},
		{"weekly", map[string]any{"ok": false}, false, true, 1, "失败"},
		{"daily", map[string]any{"ok": false}, true, false, 0, ""},
		{"daily", map[string]any{"ok": true}, false, false, 0, ""},
		{"keepalive", map[string]any{"ok": true}, true, true, 0, ""},
		{"tasks", map[string]any{"ok": true}, true, true, 0, ""},
	} {
		messages = nil
		cfg.Events.TaskDone = tc.done
		cfg.Events.TaskFail = tc.fail
		s.notifyJobDone(&jobNotify{Name: tc.name, Label: tc.name, Account: "all", Result: tc.result})
		if len(messages) != tc.want {
			t.Fatalf("%s messages=%d want=%d", tc.name, len(messages), tc.want)
		}
		if tc.want > 0 && (!strings.Contains(messages[0].Content, tc.status) || !strings.Contains(messages[0].Content, "—（未知）") || !strings.Contains(messages[0].Content, "fake-a：123") || !strings.Contains(messages[0].Content, "已知部分合计：123") || strings.Contains(messages[0].Content, "99999")) {
			t.Fatalf("wrong status or stale balance: name=%s status=%q content=%q", tc.name, tc.status, messages[0].Content)
		}
	}
	if queried["fake-a"] != 2 || queried["fake-b"] != 2 {
		t.Fatalf("fresh per-account queries=%v", queried)
	}
	// Full success reports the actual sum once.
	// 注意：新站是首选取分路径，全部成功场景也要把新站 fixture 一起换成有效值。
	newGrowNotifySession = func(dir string) (growNotifySession, error) {
		if filepath.Base(filepath.Dir(dir)) == "fake-a" {
			return beanFixture{value: 123}, nil
		}
		return beanFixture{value: 77}, nil
	}
	newNotifySession = func(dir string) (notifyBeanSession, error) {
		if filepath.Base(filepath.Dir(dir)) == "fake-a" {
			return beanFixture{value: 123}, nil
		}
		return beanFixture{value: 77}, nil
	}
	messages = nil
	cfg.Events.TaskDone = true
	s.notifyJobDone(&jobNotify{Name: "daily", Label: "daily", Account: "all", Result: map[string]any{"ok": true}})
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "合计：200") || !strings.Contains(messages[0].Content, "fake-b：77") {
		t.Fatal("wrong full sum")
	}
	// Explicit all-false events must survive disk loading.
	os.MkdirAll(filepath.Join(config.Root(), "state"), 0700)
	os.WriteFile(pushPlusConfigPath(), []byte(`{"enabled":true,"token":"FAKE_TEST_ONLY","events":{"task_done":false,"task_fail":false,"login_expired":false}}`), 0600)
	if loaded := loadPushPlusConfig(); loaded.Events.TaskDone || loaded.Events.TaskFail || loaded.Events.LoginExpired {
		t.Fatal("explicit false overwritten")
	}
	pushCfg = loadPushPlusConfig()
	messages = nil
	s.notifyJobDone(&jobNotify{Name: "daily", Result: map[string]any{"ok": true}})
	if len(messages) != 0 {
		t.Fatal("persisted all-false sent notification")
	}
	pushCfg = cfg
	cfg.Enabled = false
	s.notifyJobDone(&jobNotify{Name: "weekly", Result: map[string]any{"ok": true}})
	if len(messages) != 0 {
		t.Fatal("disabled notification sent")
	}
	cfg.Enabled = true
	// Errors must never echo platform bodies/messages, including reflected credentials.
	http.DefaultTransport = notifyMock(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"code":400,"msg":"FAKE_TEST_ONLY"}`)), Header: make(http.Header)}, nil
	})
	if err := sendPushPlus(cfg.Token, "test", "test", ""); err == nil || strings.Contains(err.Error(), cfg.Token) {
		t.Fatal("unsafe platform error")
	}
	http.DefaultTransport = notifyMock(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader("FAKE_TEST_ONLY invalid JSON")), Header: make(http.Header)}, nil
	})
	if err := sendPushPlus(cfg.Token, "test", "test", ""); err == nil || strings.Contains(err.Error(), cfg.Token) {
		t.Fatal("unsafe JSON error")
	}
}
