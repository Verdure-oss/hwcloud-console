package huawei

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeGrow 模拟新成长中心接口，验证客户端契约与错误处理。
func TestGrowClientContract(t *testing.T) {
	var signCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/hdgrowservice/user/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"id": "u1", "domain_id": "d1", "total_points": 1000, "signed": false,
		}})
	})
	mux.HandleFunc("/api/v1/hdgrowservice/user/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("tasks must be POST, got %s", r.Method)
		}
		fin := true
		unfin := false
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"records": []map[string]any{
				{"id": "1", "task_tag": TagDailyLogin, "cycle_type": "DAILY", "is_finish": fin, "points": 10},
				{"id": "2", "task_tag": TagGrapeOpenSource, "cycle_type": "LIMIT", "is_finish": unfin, "points": 50},
			},
		}})
	})
	mux.HandleFunc("/api/v1/hdgrowservice/user/sign-in", func(w http.ResponseWriter, r *http.Request) {
		signCalls++
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"success": true, "points": 10, "total_points": 1010, "signed": true,
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	apiBase := srv.URL + "/api/v1/hdgrowservice"

	// 用测试服务器替换 base：直接构造 client 指向 srv。
	dir := t.TempDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "cookies.json"), []byte(`[{"name":"koocdp_sid","value":"x","domain":"grow.developer.huaweicloud.com","path":"/"}]`), 0o600)
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"user_agent":"test-ua"}`), 0o600)

	s, err := NewGrowSessionForAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UserInfoAt(apiBase)
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalPoints != 1000 || u.Signed {
		t.Fatalf("unexpected user: %+v", u)
	}
	tasks, err := s.TasksAt(apiBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || !tasks[0].Finished() || tasks[1].Finished() {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
	got := map[string]bool{}
	for _, task := range tasks {
		got[task.TaskTag] = task.Finished()
	}
	if !got[TagDailyLogin] || got[TagGrapeOpenSource] {
		t.Fatalf("tag status wrong: %+v", got)
	}
	r, err := s.SignInAt(apiBase)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Signed || r.TotalPoints != 1010 || signCalls != 1 {
		t.Fatalf("unexpected sign-in: %+v calls=%d", r, signCalls)
	}
}

// TestGrowNotLoggedIn 验证 401 会被识别为登录过期而不是解析错误。
func TestGrowNotLoggedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":10001,"message":"未登录或登录已过期","data":null}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cookies.json"), []byte(`[]`), 0o600)
	s, err := NewGrowSessionForAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserInfoAt(srv.URL); err != ErrGrowNotLoggedIn {
		t.Fatalf("want ErrGrowNotLoggedIn, got %v", err)
	}
}

// TestGrowAPIError 验证业务 code != 0 时返回 GrowAPIError。
func TestGrowAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":20022,"message":"超出兑换上限","data":null}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cookies.json"), []byte(`[]`), 0o600)
	s, err := NewGrowSessionForAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UserInfoAt(srv.URL)
	apiErr, ok := err.(*GrowAPIError)
	if !ok || apiErr.Code != 20022 {
		t.Fatalf("want GrowAPIError 20022, got %v", err)
	}
}
