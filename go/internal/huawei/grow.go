package huawei

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hwcloud/internal/config"
)

// 新版成长中心（2026-11 上线）。
//
// 旧后端 devdata.huaweicloud.com/rest/developer/... 已于 2026-11-23 前后下线：
//   - GET /api/get-ainfo      → 404 no Route matched
//   - 签到 / 任务 / 积分接口  → 403 not login / 404
// 新后端是 grow-dataset.developer.huaweicloud.com/api/v1/hdgrowservice，
// 站点是 grow.developer.huaweicloud.com（Vue SPA）。
//
// 会话方式：Cookie（withCredentials）。登录必须走 auth.huaweicloud.com 的
// 账密 + 可选短信验证，成功后 302 回 grow 并种下 koocdp_sid 等 cookie。
// 登录由 hwlogin（Camoufox）完成，这里只负责用导出的 cookie 调接口。

const (
	GrowBase       = "https://grow.developer.huaweicloud.com"
	GrowAPIBase    = "https://grow-dataset.developer.huaweicloud.com/api/v1/hdgrowservice"
	GrowLoginURL   = "https://auth.huaweicloud.com/authui/login.html?service=https%3A%2F%2Fgrow.developer.huaweicloud.com&locale=zh-cn"
	growAPITimeout = 25 * time.Second
)

// GrowTask 是新版成长中心的任务（/user/tasks 与 /servlet/tasks 共有字段）。
type GrowTask struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	URL            string  `json:"url"`
	Points         int     `json:"points"`
	CycleType      string  `json:"cycle_type"` // DAILY / WEEKLY / MONTHLY / LIMIT
	TaskTag        string  `json:"task_tag"`
	CategoryID     string  `json:"category_id"`
	CategoryName   string  `json:"category_name"`
	TaskLimit      *int    `json:"task_limit"`
	IsFinish       *bool   `json:"is_finish"`
	CompletedTimes *int    `json:"completed_times"`
	StartTime      string  `json:"start_time"`
	EndTime        string  `json:"end_time"`
	Enable         bool    `json:"enable"`
	BizType        *string `json:"biz_type"`
}

// Finished 报告该任务当前周期是否已完成。
//
// 公共任务列表（/servlet/tasks）没有 is_finish，返回 false；
// 个人任务列表（/user/tasks）才有真实状态。
func (t GrowTask) Finished() bool {
	return t.IsFinish != nil && *t.IsFinish
}

// GrowUser 是 /user/info 返回的用户信息（只保留用到的字段）。
type GrowUser struct {
	ID          string `json:"id"`
	DomainID    string `json:"domain_id"`
	IAMID       string `json:"iam_id"`
	DomainName  string `json:"domain_name"`
	IAMName     string `json:"iam_name"`
	UserType    string `json:"user_type"`
	TotalPoints int    `json:"total_points"`
	Signed      bool   `json:"signed"`
}

// SignResult 是 /user/sign-in 的返回。
type SignResult struct {
	Success     bool `json:"success"`
	Points      int  `json:"points"`
	TotalPoints int  `json:"total_points"`
	Signed      bool `json:"signed"`
}

// growEnvelope 是统一响应壳：{code, message, data}。
type growEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// GrowSession 用账号 state 目录里的 cookie 访问新版成长中心。
//
// 为什么不用 huawei.Session：新站只认 Cookie（不认旧 devdata 的 csrf 头），
// 而且旧 Session 的 do() 把 URL 硬编码到了 devdata。独立实现能避免两套
// 后端互相污染，旧代码也还能在旧接口上继续工作（如果恢复的话）。
type GrowSession struct {
	client *http.Client
	UA     string
}

// NewGrowSessionForAccount 用指定账号的 state 目录构造会话。
//
// dir 是 state 目录（如 /opt/hwcloud-demo/accounts/<id>/state）。
func NewGrowSessionForAccount(dir string) (*GrowSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	s := &GrowSession{
		client: &http.Client{
			Jar:     jar,
			Timeout: growAPITimeout,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
				ForceAttemptHTTP2: true,
			},
		},
		UA: growUserAgentIn(dir),
	}
	if err := loadGrowCookies(jar, filepath.Join(dir, "cookies.json")); err != nil {
		return nil, err
	}
	return s, nil
}

// NewGrowSession 用当前账号（config.StateDir）构造会话。
func NewGrowSession() (*GrowSession, error) {
	return NewGrowSessionForAccount(config.StateDir())
}

func growUserAgentIn(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if eq := 0; err == nil && eq == 0 {
		var meta map[string]any
		if json.Unmarshal(data, &meta) == nil {
			if v, ok := meta["user_agent"].(string); ok && v != "" {
				return v
			}
		}
	}
	return config.DefaultUA
}

// loadGrowCookies 把 cookies.json（[]cookie 或 {"cookies":[]}）导入 jar。
func loadGrowCookies(jar http.CookieJar, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	type cookie struct {
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		Domain  string  `json:"domain"`
		Path    string  `json:"path"`
		Secure  bool    `json:"secure"`
		Expires float64 `json:"expires"`
	}
	var list []cookie
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapper struct {
			Cookies []cookie `json:"cookies"`
		}
		if err2 := json.Unmarshal(raw, &wrapper); err2 != nil {
			return err
		}
		list = wrapper.Cookies
	}
	byDomain := map[string][]*http.Cookie{}
	for _, c := range list {
		if c.Name == "" || c.Domain == "" {
			continue
		}
		host := strings.TrimPrefix(c.Domain, ".")
		p := c.Path
		if p == "" {
			p = "/"
		}
		ck := &http.Cookie{Name: c.Name, Value: c.Value, Path: p, Domain: c.Domain, Secure: c.Secure}
		if c.Expires > 0 {
			ck.Expires = time.Unix(int64(c.Expires), 0)
		}
		byDomain[host] = append(byDomain[host], ck)
	}
	for host, cs := range byDomain {
		byPath := map[string][]*http.Cookie{}
		for _, c := range cs {
			byPath[c.Path] = append(byPath[c.Path], c)
		}
		for p, group := range byPath {
			jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: p}, group)
		}
	}
	return nil
}

// SaveGrowCookies 把 jar 里的 cookie 写回账号 state 目录。
func (s *GrowSession) SaveGrowCookies(dir string) error {
	hosts := []string{
		"grow.developer.huaweicloud.com",
		"grow-dataset.developer.huaweicloud.com",
		"developer.huaweicloud.com",
		"auth.huaweicloud.com",
		"id1.cloud.huawei.com",
		"devdata.huaweicloud.com",
		"harmonybp.developer.huaweicloud.com",
		"hands-on-agent.developer.huaweicloud.com",
	}
	type outCookie struct {
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		Domain  string  `json:"domain"`
		Path    string  `json:"path"`
		Secure  bool    `json:"secure"`
		Expires float64 `json:"expires"`
	}
	var out []outCookie
	seen := map[string]bool{}
	for _, host := range hosts {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		for _, c := range s.client.Jar.Cookies(u) {
			key := c.Name + "|" + host + "|" + c.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			exp := float64(-1)
			if !c.Expires.IsZero() {
				exp = float64(c.Expires.Unix())
			}
			out = append(out, outCookie{Name: c.Name, Value: c.Value, Domain: host, Path: c.Path, Secure: c.Secure, Expires: exp})
		}
	}
	if len(out) == 0 {
		return fmt.Errorf("没有可保存的 cookie")
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cookies.json"), b, 0o600)
}

// call 发一个 JSON 请求并解包统一响应壳。
func (s *GrowSession) call(method, path string, body any, out any) error {
	return s.callAt(GrowAPIBase, method, path, body, out)
}

// callAt 同 call，但可指定 base（测试注入 httptest.Server）。
func (s *GrowSession) callAt(base, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.UA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", GrowBase)
	req.Header.Set("Referer", GrowBase+"/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求成长中心失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrGrowNotLoggedIn
	}
	var env growEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("成长中心响应解析失败（HTTP %d）: %s", resp.StatusCode, clipGrow(string(raw)))
	}
	if env.Code != 0 {
		return &GrowAPIError{Code: env.Code, Message: env.Message}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("成长中心 data 解析失败: %w", err)
		}
	}
	return nil
}

// ErrGrowNotLoggedIn 表示新站会话失效（HTTP 401 / code 10001）。
var ErrGrowNotLoggedIn = fmt.Errorf("成长中心未登录或会话已过期")

// GrowAPIError 是业务错误（HTTP 200 但 code != 0）。
type GrowAPIError struct {
	Code    int
	Message string
}

func (e *GrowAPIError) Error() string {
	return fmt.Sprintf("成长中心返回 code=%d: %s", e.Code, e.Message)
}

func clipGrow(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// UserInfo 拉取当前登录用户（同时用于探活）。
func (s *GrowSession) UserInfo() (*GrowUser, error) {
	return s.UserInfoAt(GrowAPIBase)
}

// UserInfoAt 同 UserInfo，可指定 base（测试用）。
func (s *GrowSession) UserInfoAt(base string) (*GrowUser, error) {
	var u GrowUser
	if err := s.callAt(base, http.MethodGet, "/user/info", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Tasks 拉取当前用户的任务列表（含完成状态）。
func (s *GrowSession) Tasks() ([]GrowTask, error) {
	return s.TasksAt(GrowAPIBase)
}

// TasksAt 同 Tasks，可指定 base（测试用）。
func (s *GrowSession) TasksAt(base string) ([]GrowTask, error) {
	var data struct {
		Records []GrowTask `json:"records"`
		Total   int        `json:"total"`
	}
	if err := s.callAt(base, http.MethodPost, "/user/tasks", map[string]any{
		"page_no": 1, "page_size": 100,
	}, &data); err != nil {
		return nil, err
	}
	return data.Records, nil
}

// SignIn 执行签到。重复签到由服务端返回 success=false，不视为错误。
func (s *GrowSession) SignIn() (*SignResult, error) {
	return s.SignInAt(GrowAPIBase)
}

// SignInAt 同 SignIn，可指定 base（测试用）。
func (s *GrowSession) SignInAt(base string) (*SignResult, error) {
	var r SignResult
	if err := s.callAt(base, http.MethodPost, "/user/sign-in", map[string]any{}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Points 返回当前总积分。
func (s *GrowSession) Points() (int, error) {
	u, err := s.UserInfo()
	if err != nil {
		return 0, err
	}
	return u.TotalPoints, nil
}

// SignedToday 判断今日是否已签到。
//
// 不能用 /user/info 的 signed 字段：2026-11-23 实测该接口根本不返回
// signed（keys 只有 id / domain_id / domain_name / total_points /
// user_type / created_at / updated_at 等），Go 反序列化后永远是 false。
// 平台自己给出的权威判据是任务清单里的 daily_login 任务是否已完成。
func (s *GrowSession) SignedToday() (bool, error) {
	tasks, err := s.Tasks()
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if t.TaskTag == TagDailyLogin {
			return t.Finished(), nil
		}
	}
	// 任务清单里没有签到项：退化为「不知道」，由调用方决定是否尝试签到。
	return false, fmt.Errorf("任务清单中没有 %s 项", TagDailyLogin)
}

// publicTasks 拉公共任务清单（不需要登录）。仅用于做映射/排查。
func (s *GrowSession) publicTasks() ([]GrowTask, error) {
	req, err := http.NewRequest(http.MethodGet, GrowAPIBase+"/servlet/tasks?show=true&biz_type=opc&page_no=1&page_size=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.UA)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env growEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	var data struct {
		Records []GrowTask `json:"records"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, err
	}
	return data.Records, nil
}

// TaskByTag 按 task_tag 找任务（个人列表优先，找不到再查公共清单）。
func (s *GrowSession) TaskByTag(tag string) (*GrowTask, error) {
	if list, err := s.Tasks(); err == nil {
		for i := range list {
			if list[i].TaskTag == tag {
				return &list[i], nil
			}
		}
	}
	list, err := s.publicTasks()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].TaskTag == tag {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("未找到任务 tag=%s", tag)
}

// 任务 tag 常量（实测自 2026-11-23 的 /servlet/tasks）。
const (
	TagDailyLogin        = "daily_login"
	TagDevSpace          = "ai_devspace_try"
	TagStarPracticeRepo  = "star_practice_repo"
	TagIssuePracticeRepo = "issue_feedback_practice_repo"
	TagGrapeOpenSource   = "grape_open_source"
	TagPublishGallery    = "publish_work_gallery"
	TagICTInnovation     = "innovation_contest_signup"
	TagHMDailyCaseView   = "hm_daily_case_view"
	TagHMWeeklyCase      = "hm_weekly_case_experience"
	TagHMStarCaseRepo    = "hm_star_case_repo"
)

// GrowTags 便于按周期归类做批量预检。
var GrowTags = map[string][]string{
	"daily": {
		TagDailyLogin, TagDevSpace, TagHMDailyCaseView, TagHMStarCaseRepo,
	},
	"weekly": {
		TagStarPracticeRepo, TagIssuePracticeRepo, TagHMWeeklyCase,
	},
	"monthly": {
		TagPublishGallery,
	},
}

// GrowTagTitle 把 tag 映射成中文标题（给日志和通知用）。
func GrowTagTitle(tag string) string {
	switch tag {
	case TagDailyLogin:
		return "每日签到"
	case TagDevSpace:
		return "体验 AI DevSpace"
	case TagStarPracticeRepo:
		return "每周Star一个Agent开发实践中心代码仓"
	case TagIssuePracticeRepo:
		return "每周给Agent开发实践中心代码仓提 Issue"
	case TagGrapeOpenSource:
		return "Star开源项目Grape代码仓"
	case TagPublishGallery:
		return "在开发者作品展览馆中发布作品"
	case TagICTInnovation:
		return "报名华为ICT大赛创新赛赛事"
	case TagHMDailyCaseView:
		return "每日浏览一个鸿蒙案例"
	case TagHMWeeklyCase:
		return "每周实践一次鸿蒙案例"
	case TagHMStarCaseRepo:
		return "Star一个鸿蒙案例代码仓"
	}
	return tag
}

// growMu 串行化“同一账号的签到/读状态”，避免并发重复签到。
var growMu sync.Mutex

// GrowSync 是给面板/定时任务用的高层入口：拉状态、签到、返回结果摘要。
//
// 注意：它只做「新站本身能完成」的事（签到 + 读任务状态），
// 需要浏览页面/仓库的任务由 task 层各业务完成。
func GrowSync() (map[string]any, error) {
	growMu.Lock()
	defer growMu.Unlock()

	s, err := NewGrowSession()
	if err != nil {
		return nil, err
	}
	u, err := s.UserInfo()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"user_id":      u.ID,
		"domain_id":    u.DomainID,
		"total_points": u.TotalPoints,
		"signed":       u.Signed,
	}
	if !u.Signed {
		sign, err := s.SignIn()
		if err != nil {
			out["sign_error"] = err.Error()
		} else {
			out["sign"] = sign
			out["signed"] = sign.Signed
			out["total_points"] = sign.TotalPoints
		}
	}
	tasks, err := s.Tasks()
	if err != nil {
		out["tasks_error"] = err.Error()
		return out, nil
	}
	var done, todo []string
	for _, t := range tasks {
		if t.Finished() {
			done = append(done, t.TaskTag)
		} else {
			todo = append(todo, t.TaskTag)
		}
	}
	out["tasks_done"] = done
	out["tasks_todo"] = todo
	out["tasks_total"] = len(tasks)
	return out, nil
}
