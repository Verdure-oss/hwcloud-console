package task

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

var sectionKeys = []string{"背景与问题", "现状核实", "建议方案", "预期收益"}

// LoginGitCode 被动轮询 localStorage，不做页面跳转或刷新。
func LoginGitCode(timeoutSec int) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	if err := os.MkdirAll(config.GitCodeProfileDir(), 0o755); err != nil {
		return nil, err
	}
	// UA 必须是 Chrome 的（见 LoginGitCodeAuto 的说明），否则 WAF 418。
	ctx, err := browser.LaunchPersistentUA(pw, config.GitCodeProfileDir(), false, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer ctx.Close()

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = ctx.NewPage()
		if err != nil {
			return nil, err
		}
	}
	if _, err := page.Goto("https://gitcode.com/login", playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return nil, err
	}
	fmt.Println("[*] 请登录你已绑定华为云账号的 GitCode 账号（首次使用需先在实践中心完成绑定）。")
	fmt.Println("[*] 本窗口不会自动刷新或跳转，登录完成后自动保存会话。")

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var rawUser any
	for time.Now().Before(deadline) {
		if page.IsClosed() {
			return nil, fmt.Errorf("GitCode 登录窗口被关闭")
		}
		if v, err := page.Evaluate("() => localStorage.getItem('userInfo')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				rawUser = s
				break
			}
		}
		if v, err := page.Evaluate("() => localStorage.getItem('refresh_token')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if rawUser == nil {
		return nil, fmt.Errorf("超时未检测到 GitCode 登录态")
	}

	time.Sleep(2 * time.Second)
	if _, err := ctx.StorageState(playwright.BrowserContextStorageStateOptions{
		Path: playwright.String(config.GitCodeStateFile()),
	}); err != nil {
		return nil, err
	}
	user := map[string]any{}
	if s, ok := rawUser.(string); ok {
		_ = json.Unmarshal([]byte(s), &user)
	}
	username := ""
	if v, err := page.Evaluate("() => localStorage.getItem('avatar-visited-username') || ''"); err == nil {
		if s, ok := v.(string); ok {
			username = s
		}
	}
	result := map[string]any{"id": user["id"], "username": username}
	fmt.Printf("[+] GitCode 登录成功：%v\n", username)
	fmt.Printf("[+] 会话已保存：%s\n", config.GitCodeStateFile())
	return result, nil
}

func ParseIssueSections(text string) map[string]string {
	sections := map[string]string{}
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			buf = nil
			continue
		}
		buf = append(buf, line)
	}
	flush()
	if len(sections) == 0 {
		sections["背景与问题"] = strings.TrimSpace(text)
	}
	return sections
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// CreateIssue 在 eduSwarm 仓库创建 Issue。
//
// headless 参数保留但**不再用于无头模式**：实测 GitCode 对无头浏览器
// 做了行为判定——访问 /issues/create 会被 302 回 issues 列表，
// 表现成「未进入 Issue 编辑页，会话可能失效」，误导排查。
// 服务器有 Xvfb（DISPLAY=:99），统一走有头模式。
// CreateIssue 创建 Issue，失败时整体重试。
//
// OUTER_RETRY_FIX（2026-11-26）
//
// GitCode 的 Issue 创建页对自动化很不友好，实测同一份代码多次运行结果不同：
//   - 有时正常进模板页，能点到「Use this template」并填完 4 段
//   - 有时模板卡片压根不渲染（页面只剩 [中][EN][Watch][Fork]，
//     正文提示 "This repository does not support blank issue templates"）
//   - 有时点完模板又被 SPA 弹回 /issues/create/choose
// 这些都是它自己前端的状态问题，不是代码能「一次看懂」的。
//
// 所以策略改成：把整条流程（导航 → 选模板 → 填表 → 提交）当成一个可重试单元，
// 最多试 3 轮，任一轮成功即返回。每轮之间重新导航，让前端状态重来一次。
func CreateIssue(title, body, template string, headless, dryRun bool) (map[string]any, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := createIssueOnce(title, body, template, headless, dryRun)
		if err == nil {
			if attempt > 1 {
				fmt.Printf("[*] 第 %d 轮尝试成功\n", attempt)
			}
			return res, nil
		}
		lastErr = err
		fmt.Printf("[!] 第 %d/%d 轮失败: %v\n", attempt, maxAttempts, err)
		if attempt < maxAttempts {
			fmt.Printf("[*] 重新导航后重试…\n")
		}
	}
	return nil, fmt.Errorf("连续 %d 轮都失败（GitCode 前端状态不稳定），最后一次错误: %w",
		maxAttempts, lastErr)
}

func createIssueOnce(title, body, template string, headless, dryRun bool) (map[string]any, error) {
	_ = headless
	headless = false
	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return nil, fmt.Errorf("未找到 GitCode 会话，请先运行 `hwcloud login-gitcode`")
	}
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewStorageContextUA(pw, state, headless, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}

	// 先访问首页，让前端用 refresh_token 换新 access_token。
	//
	// 为什么必须这一步：
	//   保存的 storage_state 里 access_token 的有效期只有 ~24 小时，
	//   过期后直接打开 /issues/create 会被 302 回 issues 列表，
	//   前端看起来像「会话失效」，实际 refresh_token 还有效（能用到几个月后）。
	//   GitCode 前端在页面加载时会自动调
	//   GET /uc/api/v1/user/oauth/token 刷新，所以先落地首页即可。
	//   实测：不访问首页直接进新建页 100% 失败；访问首页后立刻成功。
	if _, err := page.Goto("https://gitcode.com", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	if _, err := page.Goto(config.GitCodeIssuesNew, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(6000)

	// 等跳转完成：/issues/create → /issues/create/choose（模板选择页）。
	//
	// 坑：这一步是前端路由跳转，NetworkIdle 之后还要再等 JS 渲染。
	// 实测 6 秒常常不够，页面还停在 /issues 列表，于是误报「会话失效」。
	// 所以这里轮询等，直到 URL 变成 choose 或出现标题输入框。
	for i := 0; i < 30; i++ {
		u := page.URL()
		if strings.Contains(u, "choose") || strings.Contains(u, "issues/create") {
			break
		}
		if count(page.Locator(`input[placeholder*="标题"], input[placeholder*="title" i]`)) > 0 {
			break
		}
		page.WaitForTimeout(1000)
	}
	fmt.Printf("[*] Issue 页面: %s\n", page.URL())

	// CREATE_RETRY_FIX
	//
	// 2026-11-26 实测：GitCode 的 Issue 创建页时好时坏 ——
	// 同一份代码，有时正常进 choose 页，有时停在空白/列表页，
	// 标题框和编辑器都不出现（FILL_DIAG 报「读标题超时」「编辑器总数=0」）。
	// 这属于前端路由偶发失败，不是会话问题（会话有效时也会这样）。
	//
	// 处理方式：最多重新导航 3 次，直到出现 choose 页或标题框。
	reached := func() bool {
		u := page.URL()
		if strings.Contains(u, "choose") {
			return true
		}
		if count(page.Locator(`input[placeholder*="标题"], input[placeholder*="title" i]`)) > 0 {
			return true
		}
		return false
	}
	for attempt := 1; attempt <= 3 && !reached(); attempt++ {
		fmt.Printf("[*] 页面未就绪，第 %d 次重试导航…\n", attempt)
		_, _ = page.Goto(config.GitCodeIssuesNew, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(60000),
		})
		page.WaitForTimeout(5000)
		for i := 0; i < 20; i++ {
			if reached() {
				break
			}
			page.WaitForTimeout(1000)
		}
		fmt.Printf("[*] 重试 %d 后 URL: %s\n", attempt, page.URL())
	}

	// 模板选择页：页面列出仓库里配置的 Issue 模板，必须点「使用该模板」才能进编辑器。
	// 实测按钮顺序：0=Bug 报告，1=功能建议，后面是「打开查看」类文档链接。
	//
	// 必须用 button:has-text() 定位，不能用 GetByText(...Exact)。页面上
	// 「使用该模板」这串文字同时出现在外层 DIV 和内层 STRONG 上，GetByText
	// 会命中 DIV —— 点它不触发任何事件，页面原地不动，最后报「未进入
	// Issue 编辑页」，看起来像会话失效，实际是点错了元素。
	// ISSUE_TEMPLATE_FIX
	//
	// 2026-11-25 实测失败：页面停在 /issues/create/choose，
	// 按钮列表里明明有 [Use this template] ×2，日志却报「找不到 Issue 提交按钮」。
	// 根因不是找不到模板按钮，而是点了之后没进编辑器，后面一路往下走到
	// 「找提交按钮」才报错，把真实原因盖住了。
	//
	// 现在：
	//   1. 模板按钮逐个尝试（索引可能因模板顺序变化而错位）
	//   2. 每次点击后确认是否真的进了编辑器
	//   3. 全都失败时，报「点了模板但没进编辑器」，而不是误导性的「找不到提交按钮」
	entryReady := func() bool {
		if count(page.Locator(`input[placeholder*="标题"], input[placeholder*="title" i]`)) > 0 {
			return true
		}
		// 有些版本编辑器用 contenteditable 而不带 placeholder
		if count(page.Locator(`.cm-content`)) > 0 {
			return true
		}
		return false
	}

	if strings.Contains(page.URL(), "choose") {
		// ANY_ELEMENT_SELECTOR
		//
		// 2026-11-26 实测：模板页上确实显示「Use this template」，
		// 但 button:has-text() 匹配不到 —— 这些模板卡片是 <a> 或 <div>，
		// 不是 <button>。之前只兜底到 :is(button,a,div) 仍失败，
		// 因为 has-text 对同一段文字命中多个外层容器（含整个页面），
		// 取到的是不可点的那个。
		//
		// 现在按「元素类型 + 文本精确度」多轮尝试：
		//   1. a / button 这类本身就是可点控件的
		//   2. div 里文本等于目标串的最内层
		var buttons playwright.Locator
		var n int
		for _, sel := range []string{
			`a:has-text("使用该模板")`,
			`a:has-text("Use this template")`,
			`button:has-text("使用该模板")`,
			`button:has-text("Use this template")`,
			`[role="button"]:has-text("Use this template")`,
			`[role="button"]:has-text("使用该模板")`,
		} {
			loc := page.Locator(sel)
			if c, err := loc.Count(); err == nil && c > 0 {
				buttons = loc
				n = c
				break
			}
		}
		if n == 0 {
			// 最后兜底：任何含目标文本的最内层元素（用 :not(:has(...)) 排除容器）
			for _, txt := range []string{"Use this template", "使用该模板"} {
				sel := `:is(a,button,[role="button"],span,div):has-text("` + txt + `"):not(:has(:is(a,button,[role="button"])))`
				loc := page.Locator(sel)
				if c, err := loc.Count(); err == nil && c > 0 {
					buttons = loc
					n = c
					break
				}
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("模板选择页没有「使用该模板/Use this template」按钮（页面可能改版）")
		}

		prefer := 1
		if template == "bug" {
			prefer = 0
		}
		// 顺序：偏好索引 → 其余全部，逐个试到进编辑器为止
		order := []int{}
		if prefer < n {
			order = append(order, prefer)
		}
		for i := 0; i < n; i++ {
			if i != prefer {
				order = append(order, i)
			}
		}

		entered := false
		var lastErr error
		for _, idx := range order {
			// 强制点击：模板卡片常被遮罩层盖住，普通 click 会超时
			if err := buttons.Nth(idx).Click(playwright.LocatorClickOptions{
				Timeout: playwright.Float(8000),
				Force:   playwright.Bool(true),
			}); err != nil {
				lastErr = err
				continue
			}
			// 等编辑器渲染，最多 25 秒
			for i := 0; i < 25; i++ {
				if entryReady() {
					entered = true
					break
				}
				page.WaitForTimeout(1000)
			}
			if !entered {
			}
			if entered {
				break
			}
			lastErr = fmt.Errorf("点了第 %d 个模板按钮但未进入编辑器", idx)
		}

		if !entered {
			cur := page.URL()
			if lastErr != nil {
				return nil, fmt.Errorf("点模板后未进入 Issue 编辑器（当前 %s）：%v", cur, lastErr)
			}
			return nil, fmt.Errorf("点模板后未进入 Issue 编辑器（当前 %s）", cur)
		}
	}

	// 标题输入框的 placeholder 中英文都见过，按语义兜底。
	titleInput := page.Locator(`input[placeholder="请填写 Issue 标题"], input[placeholder="Please fill in the Issue title"], input[placeholder*="标题"], input[placeholder*="title" i]`)
	if count(titleInput) == 0 {
		return nil, fmt.Errorf("未进入 Issue 编辑页（当前 %s），GitCode 会话可能已失效或入口改版", page.URL())
	}
	if err := titleInput.Fill(title); err != nil {
		return nil, err
	}
	page.WaitForTimeout(800)

	// SINGLE_EDITOR_FIX
	//
	// 2026-11-25 实测：新版 GitCode 的 Issue 模板页只有**一个** .cm-content
	// （整篇正文一个框），而 sectionKeys 仍是 4 个分节，于是逐段填充时
	// i>=1 的段全部落空，正文一段都写不进去，提交必然被前端校验拦住。
	//
	// 处理方式按编辑器数量分流：
	//   1 个  → 新站模板，把原始 markdown 整篇塞进去
	//   多个  → 旧站模板，按 sectionKeys 逐段填（保留原逻辑）
	//
	// 填充一律用 JS 直接改 textContent 并派发 input 事件：
	// CodeMirror 6 的 contenteditable 会认这个事件同步内部文档。
	// 不用 Click+Ctrl+A+InsertText —— 实测 Ctrl+A 会连带选中页面其他文本，
	// InsertText 插错位置，甚至触发 SPA 路由跳回模板选择页。
	// WAIT_EDITOR_FIX
	//
	// 2026-11-25 实测：点模板后「标题框出现」不等于「正文编辑器已渲染」。
	// 标题框在表单头部先出来，.cm-content 要再等一会儿（实测 0-6 秒不等），
	// 之前直接用 Count() 拿到 0 就判失败，属于时序误判。
	// 这里轮询等 .cm-content 出现，最多 20 秒。
	nEditor := 0
	for wait := 0; wait < 20; wait++ {
		nEditor, _ = page.Locator(`.cm-content`).Count()
		if nEditor > 0 {
			break
		}
		page.WaitForTimeout(1000)
	}
	if nEditor == 0 {
		return nil, fmt.Errorf("Issue 编辑器未加载（.cm-content 数量为 0），GitCode 页面可能未渲染出模板编辑器")
	}

	// CM6_FIX
	//
	// 直接 el.textContent = t 对 CodeMirror 6 无效：CM 有自己的文档模型
	// （EditorState），DOM 只是投影，改写 DOM 后它会立刻按内部状态重绘回去，
	// 于是读回来长度还是 0（实测 4 个编辑器全是 0）。
	//
	// 正确做法二选一：
	//   A. 走 CM 的 API：.cm-editor → .cm-content 的 view 对象上 dispatch
	//   B. 用 Playwright 的 pressSequentially / type 模拟真实键盘输入
	// 这里优先 A（快、一次到位），失败退回 B（慢但一定生效）。
	// PLAYWRIGHT_FILL_FIX
	//
	// 2026-11-25 实测定案。这个 CodeMirror 6 实例的特殊之处：
	//   - 没有暴露 cmView（拿不到 EditorView.dispatch）
	//   - 改 el.textContent 会被立刻重绘覆盖
	//   - innerText 只反映**视口内已渲染的行**（CM 虚拟滚动），
	//     所以「读回长度」永远偏小甚至为 0，不能用它判断成败
	//
	// 可行的做法：Playwright 的 Locator.Fill() 对 contenteditable 走的是
	// 「聚焦 → 全选 → 插入文本」的真实输入路径，CM 会当成正常输入接收。
	// 判断成功与否改用「填入后再读 textContent（不是 innerText）」，
	// textContent 不受虚拟滚动影响。
	setEditor := func(idx int, text string) (int, error) {
		loc := page.Locator(`.cm-content`).Nth(idx)
		if cnt, _ := loc.Count(); cnt == 0 {
			return 0, fmt.Errorf("第 %d 个编辑器不存在", idx)
		}
		_ = loc.ScrollIntoViewIfNeeded()

		// 主路径：Fill
		fillErr := loc.Fill(text, playwright.LocatorFillOptions{Timeout: playwright.Float(15000)})
		if fillErr != nil {
		} else {
		}
		if fillErr != nil {
			// 兜底：点进去 + 编辑器内全选 + InsertText
			if err := loc.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
				return 0, fmt.Errorf("点击失败: %w", err)
			}
			page.WaitForTimeout(200)
			if _, err := loc.Evaluate(`(el) => {
				el.focus();
				const sel = window.getSelection();
				const r = document.createRange();
				r.selectNodeContents(el);
				sel.removeAllRanges();
				sel.addRange(r);
				return null;
			}`, nil); err != nil {
				return 0, err
			}
			if err := page.Keyboard().InsertText(text); err != nil {
				return 0, fmt.Errorf("InsertText 失败: %w (Fill 也失败: %v)", err, fillErr)
			}
		}
		page.WaitForTimeout(600)

		// 用 textContent 读回（innerText 受虚拟滚动影响，会低估）
		v, err := loc.Evaluate(`(el) => (el.textContent || '').length`, nil)
		if err != nil {
			return 0, err
		}
		n, _ := v.(int)
		return n, nil
	}

	filled := 0
	if nEditor == 1 {
		// 新站：整篇 markdown 原样填入（body 是去掉一级标题后的正文）
		n, err := setEditor(0, body)
		if err != nil {
			return nil, fmt.Errorf("整篇正文填入失败: %w", err)
		}
		if n == 0 {
			return nil, fmt.Errorf("整篇正文填入后长度仍为 0")
		}
		filled = 1
	} else {
		// DYNAMIC_SECTIONS_FIX
		//
		// 2026-11-26 实测定案：原来按写死的 sectionKeys
		// {"背景与问题","现状核实","建议方案","预期收益"} 逐段取，
		// 但 data/issue_feature.md 的二级标题是其它名字
		// （背景 / 期望功能 / 使用场景 / 补充说明），
		// sections[key] 全部为空 → continue 全跳过 → 一段没填 → 提交被校验拦住，
		// 而日志只显示「实际填入 0 处」，完全看不出是标题名对不上。
		//
		// 现在改为：**按文档里真实的 ## 标题顺序**依次填进对应编辑器。
		// 这样写文案时用任意分节名都行，不再需要跟代码里的常量对齐。
		sections := ParseIssueSections(body)
		// 保持文档出现顺序（Go map 无序，必须从原文重新抽取）
		var keys []string
		seen := map[string]bool{}
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "## ") {
				continue
			}
			k := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			// 文档没有二级标题：把整篇塞进第一个编辑器
			keys = []string{"(全文)"}
			sections["(全文)"] = strings.TrimSpace(body)
		}

		for i, key := range keys {
			if i >= nEditor {
				continue
			}
			text := sections[key]
			if text == "" {
				continue
			}
			n, err := setEditor(i, text)
			if err != nil {
				continue
			}
			if n > 0 {
				filled++
			} else {
			}
		}
	}
	if filled == 0 {
		return nil, fmt.Errorf("正文没填进去，提交会被校验拦住")
	}

	if dryRun {
		return map[string]any{"dry_run": true, "title": title,
			"editors": nEditor, "filled": filled, "template": template}, nil
	}

	// 注意：playwright-go 的 ExpectResponse 只接受 string / *regexp.Regexp，
	// 传 func(Response) bool 会在 newURLMatcher 里 panic（invalid urlOrPredicate）。
	//
	// 真实提交端点是
	//   POST https://web-api.gitcode.com/issuepr/api/v1/issue/<owner>%2F<repo>/issues
	// （注意末尾的 /issues，且仓库名是 URL 编码过的）。
	// 早先按 `**/issuepr/api/v1/issue**` 匹配会先命中列表 GET 接口
	// （.../issue/list → {all,closed,issues,opened}），拿到的是空列表。
	//
	// 另：Issue 一旦有参与者（评论、被指派、被 @）就**无法删除**，DELETE 返回
	// 400 {"error_message":"There are other participants"}，页面上「删除 issue」
	// 按钮也会变成 disabled。误发的 Issue 只能 PUT {state_event:"close"} 关掉。
	// 这也是为什么要靠 weeklyMissionDone 先查重 —— 发出去就收不回来了。
	// EXPECT_RESPONSE_FIX
	//
	// 2026-11-25 实测：原来的正则 `/issuepr/api/v1/issue/[^/]+/issues(\?|$)`
	// 同时匹配「创建 Issue（POST）」和「Issue 列表（GET ...?page=1）」。
	// ExpectResponse 抓先到的那个，实际拿到的是列表响应
	// {issues:[...], opened:57} —— 里面根本没有 iid 字段，
	// 于是 iid=0、url 为空，日志打印成「提Issue 成功: <nil>」，
	// 上层以为成功，成长中心却一直显示未完成。
	//
	// 现在改成手动监听 response 事件，按「POST + 路径以 /issues 结尾」筛选：
	//   - 必须 POST（列表是 GET）
	//   - 路径部分（去掉查询串）必须以 /issues 结尾（创建接口就是这样）
	var createResp map[string]any
	var createURL string
	page.On("response", func(r playwright.Response) {
		req := r.Request()
		if req.Method() != "POST" {
			return
		}
		u := r.URL()
		base := u
		if i := strings.IndexByte(u, '?'); i >= 0 {
			base = u[:i]
		}
		if !strings.Contains(base, "/issuepr/api/v1/issue/") ||
			!strings.HasSuffix(base, "/issues") {
			return
		}
		var d map[string]any
		if err := r.JSON(&d); err != nil {
			return
		}
		createResp = d
		createURL = u
	})

	if err := clickIssueSubmit(page); err != nil {
		return nil, err
	}
	// 给响应监听一点时间落地（clickIssueSubmit 已等到 URL 跳转，
	// 但响应事件可能在跳转之后才派发）
	page.WaitForTimeout(2000)

	data := createResp
	if data == nil {
		// 没抓到创建响应，但 URL 已经跳到 /issues/<n>，从 URL 兜底取 iid
		data = map[string]any{}
	}
	if createURL != "" {
		fmt.Printf("[*] 创建响应来自 %s\n", createURL)
	}

	iid := toInt(data["iid"])
	// 提交成功后前端会跳到 /issues/<iid>。响应体结构不稳定（有时包一层 data），
	// iid 拿不到就从 URL 里捞，避免「提交成功却报 iid=0」。
	if iid == 0 {
		if m := regexp.MustCompile(`/issues/(\d+)`).FindStringSubmatch(page.URL()); len(m) == 2 {
			iid = toInt(m[1])
		}
	}
	if iid == 0 {
		if inner, ok := data["data"].(map[string]any); ok {
			iid = toInt(inner["iid"])
		}
	}
	result := map[string]any{"iid": iid, "response": data}
	if iid > 0 {
		result["url"] = fmt.Sprintf("https://gitcode.com/%s/issues/%d", config.GitCodeRepo, iid)
	}
	return result, nil
}
// clickIssueSubmit 点击 Issue 编辑页的「提交」按钮。
//
// 为什么不能写死一个文案：GitCode 按浏览器语言渲染界面，
// 服务器上拿到的是**英文**（实测 2026-11：按钮是 "Create issue"），
// 而早先的代码只匹配中文「提交 Issue」，于是必然超时 ——
// 报错是 `waiting for getByText('提交 Issue')`，看着像页面没加载，
// 实际是文案对不上。
//
// 按优先级依次尝试：
//  1. 精确文案（中/英，常见几种写法）
//  2. 提交按钮的常见 class / type 特征
//  3. 编辑器区域里最后一个主按钮（提交通常在最右/最下）
func clickIssueSubmit(page playwright.Page) error {
	// 2026-11-25 实测定案的三个坑（改之前全踩了）：
	//
	// 坑1：盲点「最后一个主按钮」会点到语言切换。
	//   编辑页顶部有 [中][EN] 两个可见按钮，提交按钮在视口外没渲染，
	//   兜底逻辑取 Last() 就点中了「中」，页面毫无反应，
	//   而函数返回 nil（点击本身成功），上层以为提交完成。
	//
	// 坑2：提交按钮不在初始视口内。
	//   编辑器内容区很长，提交按钮在页面底部，不滚动的话
	//   Locator.Count() 能数到但 IsVisible() 为 false，点击会超时。
	//
	// 坑3：点了「提交」也可能被表单校验拦住。
	//   标题或正文为空时前端直接拦截，不发请求，URL 不变。
	//   所以「点击成功」不等于「提交成功」，必须验证 URL 变了。

	beforeURL := page.URL()

	// ISSUE_PROBE：先记录页面结构，帮助定位「提交按钮到底在不在」
	probe := func(tag string) {
		raw, err := page.Evaluate(`() => {
			const out = {btns: [], inputs: [], cm: 0, ed: 0, bodyLen: 0};
			document.querySelectorAll('button,[role="button"]').forEach(el => {
				const t = (el.innerText || '').trim();
				if (!t || t.length > 28) return;
				const r = el.getBoundingClientRect();
				out.btns.push(t + (r.width > 0 && r.height > 0 ? '✓' : '✗') +
					'@' + Math.round(r.y));
			});
			document.querySelectorAll('input,textarea').forEach(el => {
				const r = el.getBoundingClientRect();
				out.inputs.push((el.placeholder || el.type || '?') +
					(r.width > 0 ? '✓' : '✗') + '@' + Math.round(r.y));
			});
			out.cm = document.querySelectorAll('.cm-content').length;
			out.ed = document.querySelectorAll('[contenteditable="true"]').length;
			out.bodyLen = (document.body.innerText || '').length;
			return out;
		}`)
		if err != nil {
			fmt.Printf("[probe-%s] 失败: %v\n", tag, err)
			return
		}
		st, _ := raw.(map[string]any)
		if st == nil {
			fmt.Printf("[probe-%s] 返回值类型: %T\n", tag, raw)
			return
		}
		fmt.Printf("[probe-%s] cm=%v ed=%v bodyLen=%v\n", tag, st["cm"], st["ed"], st["bodyLen"])
		fmt.Printf("[probe-%s] 按钮: %v\n", tag, st["btns"])
		fmt.Printf("[probe-%s] 输入: %v\n", tag, st["inputs"])
		fmt.Printf("[probe-%s] URL: %s\n", tag, page.URL())
	}

	probe("点击前")

	// SEARCH_BUTTON_FIX
	// 绝不能在这里用 window.scrollTo(0, body.scrollHeight)：
	// 实测（2026-11-25）它会让 GitCode 的 SPA 直接路由回
	// /issues/create/choose（模板选择页），编辑器连同 [New issue] 按钮一起消失，
	// 于是「滚动以露出提交按钮」反而把提交按钮弄没了。
	// 正确做法：用 Locator.ScrollIntoViewIfNeeded()，只滚动到元素，不动路由。

	// 只认明确的提交文案。绝不能把「中」「EN」「Cancel」这类算进去。
	// GitCode 按浏览器语言渲染，服务器上实测是英文 "Create issue"。
	submitTexts := []string{
		"New issue",        // 2026-11-25 实测真实文案（GitCode 英文界面）
		"Create issue",
		"Submit your issue",
		"Submit issue",
		"创建 Issue",
		"创建Issue",
		"提交 Issue",
		"提交Issue",
	}

	// NATIVE_CLICK_FIX
	//
	// 2026-11-25 定案：前面绕了很多弯路（window.scrollTo 触发路由跳转、
	// JS 的 el.click() 不触发 Vue 事件处理器），其实根本不需要手动滚动 ——
	// Playwright 的 Locator.Click() 会**自动把元素滚动到可见区域**再发真实
	// 鼠标事件（mousedown → mouseup → click），这才是 Vue / CodeMirror
	// 这类框架能正确响应的输入方式。
	//
	// 提交按钮在页面最底部，靠 Click 的自动滚动即可，不用任何手动 scroll。
	var clicked string
	for _, txt := range submitTexts {
		// 限定 button 元素，避免命中正文里的同名文字
		loc := page.Locator(`button:has-text("` + txt + `")`)
		n, err := loc.Count()
		if err != nil || n == 0 {
			continue
		}
		// 从后往前找（提交按钮通常在最后一个）
		for idx := n - 1; idx >= 0; idx-- {
			b := loc.Nth(idx)
			// Click 自带滚动到元素；Force 跳过可见性/可点性预检，
			// 避免被浮层判定为遮挡而误判不可点
			if err := b.Click(playwright.LocatorClickOptions{
				Timeout: playwright.Float(10000),
				Force:   playwright.Bool(true),
			}); err != nil {
				continue
			}
			clicked = txt
			break
		}
		if clicked != "" {
			break
		}
	}

	if clicked == "" {
		// 列出当前可见按钮，方便下次排障
		var sb strings.Builder
		if all, err := page.Locator(`button`).All(); err == nil {
			for i, b := range all {
				if i >= 25 {
					break
				}
				if t, err := b.InnerText(); err == nil {
					t = strings.TrimSpace(t)
					if t != "" && len([]rune(t)) < 25 {
						vis, _ := b.IsVisible()
						mark := "✗"
						if vis {
							mark = "✓"
						}
						fmt.Fprintf(&sb, "[%s%s] ", t, mark)
					}
				}
			}
		}
		return fmt.Errorf("找不到可见的提交按钮（已滚动到底）。页面按钮(✓=可见)：%s", sb.String())
	}

	fmt.Printf("[*] 已点击提交按钮：%s，等待跳转…\n", clicked)

	// 验证真的提交了：URL 必须变成 /issues/<数字>
	// 不能只看「点击成功」—— 表单校验会静默拦住，页面原地不动。
	for i := 0; i < 30; i++ {
		u := page.URL()
		if u != beforeURL {
			if m := regexp.MustCompile(`/issues/(\d+)`).FindStringSubmatch(u); len(m) == 2 {
				fmt.Printf("[*] 提交成功，Issue #%s\n", m[1])
				return nil
			}
		}
		// 有些版本会弹 toast 但 URL 不变，多等一会儿
		page.WaitForTimeout(1000)
	}

	// AFTER_CLICK_DIAG：页面没跳转，看看它给了什么提示
	var pageNow string
	if v, e := page.Evaluate(`() => {
		const t = document.body ? document.body.innerText : '';
		// 找可能的错误提示元素
		const errs = [];
		document.querySelectorAll('[class*=error],[class*=Error],[class*=toast],[class*=message],[class*=alert],[role=alert]').forEach(el => {
			const s = (el.innerText || '').trim();
			if (s && s.length < 100) errs.push(s);
		});
		// 标题框和编辑器当前值
		const tInput = document.querySelector('input[placeholder*="title" i], input[placeholder*="标题"]');
		return JSON.stringify({
			errs: [...new Set(errs)].slice(0, 6),
			titleVal: tInput ? tInput.value.slice(0, 40) : '(no input)',
			cmTexts: [...document.querySelectorAll('.cm-content')].map(e =>
				(e.innerText || '').trim().slice(0, 30)),
			bodyHead: t.slice(0, 300)
		});
	}`); e == nil {
		pageNow, _ = v.(string)
	}

	return fmt.Errorf("点了提交按钮（%s）但 30 秒内未跳转到 Issue 页（当前 %s）\n"+
		"页面反馈: %s", clicked, page.URL(), pageNow)
}



//
// 为什么需要它：
//   LoginGitCode 只是打开登录页然后被动等用户手动操作，适合首次交互。
//   但 GitCode 的 access token 会不定期被服务端作废（cookie 的 expires
//   还写着 2027 年，实际 401 了），这时就需要重新登录。
//   每次都人工扫码太麻烦，这里改成自动填账密。
//
// 登录页结构（2026-10 实测）：
//   需要先点「密码登录」tab，才会出现账密表单：
//     input[placeholder="请填写用户名/邮箱"]
//     input[placeholder="请填写密码"]
//     button "登 录"
// FetchGitCodeUsername 从已保存的 GitCode 会话里读真实用户名。
//
// 为什么需要：GitCode **允许用手机号/邮箱登录**，所以用户在面板里
// 填手机号也能登录成功。但华为云的绑定接口要的是**用户名**
// （gitcode.com/<用户名> 里那段），传手机号会 404：
//
//	{"error":"gitcode user not found","username":"13800138000"}
//
// 用户名在好几个地方都有，最稳的是账号设置页：
//
//	https://gitcode.com/setting/account
//
// 页面上显示 "@wqa12130" 这样的文本，侧栏也有裸用户名。
// 这样用户只需填手机号+密码就能登录，用户名自动取，
// 不用他再去个人主页复制一遍。
//
// 返回的用户名**不带 @ 前缀** —— 华为云绑定接口要的是裸用户名。
func FetchGitCodeUsername() (string, error) {
	if err := browser.EnsureDriver(); err != nil {
		return "", err
	}
	pw, err := playwright.Run()
	if err != nil {
		return "", err
	}
	defer pw.Stop()

	state := config.GitCodeStateFile()
	if _, err := os.Stat(state); err != nil {
		return "", fmt.Errorf("未找到 GitCode 会话，请先登录")
	}

	br, ctx, err := browser.NewStorageContextUA(pw, state, true, config.DefaultUA)
	if err != nil {
		return "", err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return "", err
	}

	if _, err := page.Goto("https://gitcode.com/setting/account", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(45000),
	}); err != nil {
		return "", err
	}
	// 页面是 Vue 应用，@用户名要等 JS 渲染
	for i := 0; i < 20; i++ {
		page.WaitForTimeout(700)
		if n, err := page.Locator(`text=/^@[a-zA-Z0-9_-]+$/`).Count(); err == nil && n > 0 {
			break
		}
	}

	raw, err := page.Evaluate(`() => {
		const hits = [];
		document.querySelectorAll('*').forEach(el => {
			if (el.children.length > 2) return;          // 只看叶子节点
			const t = (el.innerText || '').trim();
			const m = t.match(/^@([a-zA-Z0-9_-]{2,40})$/);
			if (m) hits.push(m[1]);                       // 只取 @ 后面的部分
		});
		if (hits.length) return hits[0];

		// 兜底：账号设置页侧栏直接显示裸用户名。
		// 限定在 setting/account 页面，避免在别的页面误抓导航文字。
		if (!location.pathname.includes('/setting/account')) return '';
		const cands = [];
		document.querySelectorAll('div,span,a').forEach(el => {
			if (el.children.length > 0) return;
			const t = (el.innerText || '').trim();
			if (/^[a-zA-Z0-9_-]{3,40}$/.test(t)) cands.push(t);
		});
		return cands.length ? cands[0] : '';
	}`)
	if err != nil {
		return "", err
	}
	name, _ := raw.(string)
	// 兜底再剥一次 @：万一页面结构变化让 @ 混进来
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if name == "" {
		dump, _ := page.Locator("body").InnerText()
		return "", fmt.Errorf("没能从账号设置页读到用户名（URL: %s）。页面开头：%s",
			page.URL(), firstRunes(dump, 120))
	}
	return name, nil
}

// firstRunes 取字符串前 n 个 rune（避免按字节截断中文）。
func firstRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n])
}

func LoginGitCodeAuto(account, password string, timeoutSec int) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	if err := os.MkdirAll(config.GitCodeProfileDir(), 0o755); err != nil {
		return nil, err
	}
	// headless=false：GitCode 对无头浏览器风控更严，有头更容易过。
	//
	// UA 必须显式给 Chrome 的：config.UserAgent() 返回的是华为云登录时
	// 记下的 Firefox UA，套在 Chromium 上会 UA 与指纹不一致，
	// GitCode WAF 直接返回 418 "request has been intercepted"。
	ctx, err := browser.LaunchPersistentUA(pw, config.GitCodeProfileDir(), false, config.DefaultUA)
	if err != nil {
		return nil, err
	}
	defer ctx.Close()

	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else {
		if page, err = ctx.NewPage(); err != nil {
			return nil, err
		}
	}

	if _, err := page.Goto("https://gitcode.com/login",
		playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return nil, err
	}
	// 页面是 Vue 应用，DOM 里 login-tab 要等 JS 渲染完才出现，
	// 实测 3 秒不够（偶发拿不到 tab），这里等到元素真正出现为止。
	for i := 0; i < 20; i++ {
		if n, err := page.Locator(".login-tab").Count(); err == nil && n > 0 {
			break
		}
		page.WaitForTimeout(500)
	}
	page.WaitForTimeout(1000)
	fmt.Printf("[*] GitCode 登录页：%s\n", page.URL())

	// 1. 切到「密码登录」tab
	//
	// 坑一：Playwright 的 text= / :has-text 在这个页面上点不动——
	// 这些 tab 是 <div class="login-tab">，没有 role、没有 href，
	// 而且外层有同名容器，选择器命中的不是可点元素。只能坐标点击。
	//
	// 坑二：GitCode 页面会按浏览器语言渲染，实测服务器上拿到的是
	// 英文界面（Miniprogram / SMS Login / Password login），
	// 只匹配中文「密码登录」会永远失败。所以这里两种语言都认，
	// 再不行就按 DOM 顺序取最后一个 tab（密码登录固定排第三）。
	switched := false
	if pos, err := page.Evaluate(`() => {
		const want = ['密码登录', 'password login', 'login with password'];
		const tabs = Array.from(document.querySelectorAll('.login-tab'))
			.filter(el => {
				const r = el.getBoundingClientRect();
				return r.width > 20 && r.height > 10;
			});
		let pick = null;
		for (const el of tabs) {
			const t = (el.innerText || '').trim().toLowerCase();
			if (want.some(w => t === w || t.includes(w))) { pick = el; break; }
		}
		if (!pick && tabs.length) pick = tabs[tabs.length - 1];
		if (!pick) return null;
		const r = pick.getBoundingClientRect();
		return {x: Math.round(r.x), y: Math.round(r.y),
		        w: Math.round(r.width), h: Math.round(r.height),
		        text: (pick.innerText || '').trim()};
	}`); err == nil {
		if m, ok := pos.(map[string]any); ok {
			x := toInt(m["x"]) + toInt(m["w"])/2
			y := toInt(m["y"]) + toInt(m["h"])/2
			if err := page.Mouse().Click(float64(x), float64(y)); err == nil {
				switched = true
				fmt.Printf("[*] 已点击登录方式：%v\n", m["text"])
			}
		}
	}
	if !switched {
		for _, sel := range []string{
			"div.login-tab:has-text(\"Password login\")",
			"div.login-tab:has-text(\"密码登录\")",
		} {
			if err := page.Locator(sel).First().Click(
				playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				switched = true
				break
			}
		}
	}
	if !switched {
		// 打印现场，方便判断是改版还是没渲染完
		if dump, derr := page.Evaluate(`() => {
			const tabs = Array.from(document.querySelectorAll('.login-tab'))
				.map(el => (el.innerText || '').trim());
			return {url: location.href, title: document.title,
			        tabs: tabs, bodyLen: (document.body.innerText || '').length,
			        bodyHead: (document.body.innerText || '').slice(0, 300)};
		}`); derr == nil {
			fmt.Printf("[!] 登录页现场: %v\n", dump)
		}
		return nil, fmt.Errorf("找不到「密码登录」入口，GitCode 登录页可能改版")
	}
	page.WaitForTimeout(2500)

	// 2. 填账密。placeholder 也是双语的，按 input 类型兜底更稳。
	userSel := `input[placeholder="请填写用户名/邮箱"], input[placeholder="Please fill in Username/Email"], input.devui-input__inner[type="text"]`
	passSel := `input[placeholder="请填写密码"], input[placeholder="Please fill in Password"], input.devui-input__inner[type="password"]`
	if err := page.Locator(userSel).First().Fill(account); err != nil {
		return nil, fmt.Errorf("填用户名失败: %w", err)
	}
	if err := page.Locator(passSel).First().Fill(password); err != nil {
		return nil, fmt.Errorf("填密码失败: %w", err)
	}
	page.WaitForTimeout(600)

	// 3. 勾选两个协议复选框。
	//
	// 页面上有两个 devui-checkbox：
	//   y≈552  "By logging in or registering, you agree to User Agreement..."
	//   y≈596  "I agree to share my account, organization, repository information..."
	// 不勾选时点 Log In 什么都不会发生（前端直接 return，连网络请求都不发）。
	// 真正的 <input> 是 0x0 + opacity:0，点不到；要点 label/外层 div。
	// 用 el.click() 直接触发，比坐标更稳（不受滚动影响）。
	checked, cerr := page.Evaluate(`() => {
		const n = [];
		document.querySelectorAll('label, .devui-checkbox').forEach(el => {
			const r = el.getBoundingClientRect();
			if (r.width < 50 || r.height < 10) return;
			const t = (el.innerText || '').toLowerCase();
			if (!t.includes('agree')) return;
			const inp = el.querySelector('input[type=checkbox]');
			if (inp && inp.checked) return;
			el.click();
			n.push((el.innerText || '').trim().slice(0, 40));
		});
		return n;
	}`)
	if cerr != nil {
		fmt.Printf("[!] 勾选协议失败: %v\n", cerr)
	} else {
		fmt.Printf("[*] 已勾选协议: %v\n", checked)
	}
	page.WaitForTimeout(800)

	// 4. 点登录按钮。
	//
	// 页面上有三个候选：顶栏「登录」(64x32)、顶栏「注册」、表单里的
	// 「Log In」(398x40)。按文字匹配 + 取面积最大的那个，能稳定命中表单按钮。
	clicked := false
	if pos, err := page.Evaluate(`() => {
		const want = ['登录', '登 录', 'log in', 'login', 'sign in'];
		const cands = [];
		document.querySelectorAll('button').forEach(el => {
			const t = (el.innerText || '').trim().toLowerCase();
			const r = el.getBoundingClientRect();
			if (r.width < 40 || r.height < 20) return;
			if (!want.some(w => t === w || t.includes(w))) return;
			cands.push({x: Math.round(r.x), y: Math.round(r.y),
			            w: Math.round(r.width), h: Math.round(r.height),
			            area: r.width * r.height, text: (el.innerText||'').trim()});
		});
		if (!cands.length) return null;
		cands.sort((a, b) => b.area - a.area);
		return cands[0];
	}`); err == nil {
		if m, ok := pos.(map[string]any); ok {
			x := toInt(m["x"]) + toInt(m["w"])/2
			y := toInt(m["y"]) + toInt(m["h"])/2
			if err := page.Mouse().Click(float64(x), float64(y)); err == nil {
				clicked = true
				fmt.Printf("[*] 已点击登录按钮：%v\n", m["text"])
			}
		}
	}
	if !clicked {
		for _, sel := range []string{`button:has-text("Log In")`, `button:has-text("登")`} {
			if err := page.Locator(sel).Last().Click(
				playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				clicked = true
				break
			}
		}
	}
	if !clicked {
		return nil, fmt.Errorf("找不到登录按钮")
	}

	// 4. 等登录完成。GitCode 登录后会把 userInfo 写进 localStorage。
	//    也可能弹滑块验证 —— 那种情况只能等人工，这里给出明确提示。
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var rawUser any
	needCaptcha := false
	for time.Now().Before(deadline) {
		if page.IsClosed() {
			return nil, fmt.Errorf("登录窗口被关闭")
		}
		if v, err := page.Evaluate("() => localStorage.getItem('userInfo')"); err == nil && v != nil {
			if s, ok := v.(string); ok && s != "" {
				rawUser = s
				break
			}
		}
		// 检测滑块/验证码提示（页面可能中英文）
		if !needCaptcha {
			if n, err := page.Locator("text=验证, text=captcha, text=Verify, .captcha, [class*=captcha]").Count(); err == nil && n > 0 {
				needCaptcha = true
				fmt.Println("[!] 检测到验证码，请人工在 VNC 里完成（登录成功后会自动保存）")
			}
		}
		time.Sleep(2 * time.Second)
	}
	if rawUser == nil {
		if needCaptcha {
			return nil, fmt.Errorf("需要人工完成验证码，请用 VNC 连接 :99 手动完成")
		}
		return nil, fmt.Errorf("超时未检测到 GitCode 登录态（账密错误或被风控）")
	}

	time.Sleep(2 * time.Second)
	if _, err := ctx.StorageState(playwright.BrowserContextStorageStateOptions{
		Path: playwright.String(config.GitCodeStateFile()),
	}); err != nil {
		return nil, err
	}

	user := map[string]any{}
	if s, ok := rawUser.(string); ok {
		_ = json.Unmarshal([]byte(s), &user)
	}
	username := ""
	if v, err := page.Evaluate("() => localStorage.getItem('avatar-visited-username') || ''"); err == nil {
		if s, ok := v.(string); ok {
			username = s
		}
	}
	if username == "" {
		if s, ok := user["username"].(string); ok {
			username = s
		}
		if s, ok := user["login"].(string); ok && username == "" {
			username = s
		}
	}
	fmt.Printf("[+] GitCode 自动登录成功：%v\n", username)
	fmt.Printf("[+] 会话已保存：%s\n", config.GitCodeStateFile())
	return map[string]any{"id": user["id"], "username": username, "auto": true}, nil
}
