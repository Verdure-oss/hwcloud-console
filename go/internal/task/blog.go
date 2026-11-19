package task

import (
	"fmt"
	"strings"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

type BlogOptions struct {
	Title    string
	Summary  string
	Content  string
	Headless bool
	Draft    bool
	DryRun   bool
}

func dismissStartupModal(page playwright.Page) {
	for _, label := range []string{"继续编辑", "新建内容", "我知道了", "确定"} {
		items, err := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).All()
		if err != nil {
			continue
		}
		for _, item := range items {
			if visible(item) {
				_ = item.Click()
				page.WaitForTimeout(1500)
				return
			}
		}
	}
}

// PublishBlog 在华为云社区编辑器中发布 Markdown 文章。
func PublishBlog(opts BlogOptions) (map[string]any, error) {
	if err := browser.EnsureDriver(); err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, opts.Headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}
	if _, err := page.Goto(config.BlogEditorURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(9000)
	dismissStartupModal(page)

	if err := page.Locator("#blogTitle").Fill(opts.Title); err != nil {
		return nil, err
	}
	// BLOG_WAIT_PATCH
	// 点「Markdown格式编辑」。原来只认这一个精确文案，站点微调就会点空，
	// 后面自然找不到 iframe —— 报错却是「iframe 未加载」，误导排查方向。
	clickedMD := false
	for _, label := range []string{"Markdown格式编辑", "Markdown 格式编辑", "Markdown编辑"} {
		loc := page.GetByText(label, playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
		if n, err := loc.Count(); err == nil && n > 0 {
			if err := loc.First().Click(); err == nil {
				clickedMD = true
				break
			}
		}
	}
	if !clickedMD {
		shotBlogFail(page, "markdown-btn")
		return nil, fmt.Errorf("找不到「Markdown格式编辑」按钮（页面可能改版）")
	}
	page.WaitForTimeout(2000)

	// 原来固定等 4 秒就找 iframe，实测太短：编辑器是 iframe 内嵌的富文本
	// 组件，冷启动（尤其是刚跑完 DevSpace、浏览器还热着）常要 8-12 秒。
	// 改成轮询最多 25 秒，命中就立刻继续，不浪费等待时间。
	var frame playwright.Frame
	for i := 0; i < 25; i++ {
		for _, f := range page.Frames() {
			if strings.Contains(f.URL(), "HDMDEditorPortal") {
				frame = f
				break
			}
		}
		if frame != nil {
			break
		}
		page.WaitForTimeout(1000)
	}
	if frame == nil {
		// 失败时把页面框架 URL 全打出来 —— 否则只知道「没找到」，
		// 不知道实际加载了哪些 iframe，无法判断是改名还是没加载。
		var urls []string
		for _, f := range page.Frames() {
			urls = append(urls, f.URL())
		}
		shotBlogFail(page, "no-iframe")
		return nil, fmt.Errorf("Markdown 编辑器 iframe 未加载（等了 25s，当前页面 frames: %v）",
			trimFrameURLs(urls))
	}
	editor := frame.Locator("[contenteditable=true]").First()
	if err := editor.Click(); err != nil {
		return nil, err
	}
	if err := editor.Fill(opts.Content); err != nil {
		return nil, err
	}
	page.WaitForTimeout(5000)

	if opts.Summary != "" {
		_ = page.Locator("textarea[placeholder='请输入文章摘要']").First().Fill(opts.Summary)
	}

	if opts.DryRun {
		tags, _ := page.Locator(".tagator_tag").AllInnerTexts()
		return map[string]any{"dry_run": true, "title": opts.Title, "tags": tags}, nil
	}

	if opts.Draft {
		// ExpectResponse 的第一个参数在本版本的 playwright-go 里是
		// url 或 func(string) bool 的 URL 谓词，不是 func(Response) bool。
		// 之前传的是后者，运行时直接 panic: invalid urlOrPredicate。
		resp, err := page.ExpectResponse("**/save-draft*", func() error {
			return page.Locator("#draft-btn").Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
		}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(60000)})
		if err != nil {
			return nil, err
		}
		var data map[string]any
		_ = resp.JSON(&data)
		return map[string]any{"draft_id": data["draftID"], "response": data}, nil
	}

	if err := page.Locator("#agreementBox").First().Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)}); err != nil {
		return nil, err
	}
	page.WaitForTimeout(1000)

	resp, err := page.ExpectResponse("**/blog/blogging*", func() error {
		return page.Locator("#submit-btn").Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(60000)})
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		return nil, err
	}
	blogID, _ := data["blogId"].(string)
	result := map[string]any{"blog_id": blogID, "response": data}
	if blogID != "" {
		result["url"] = "https://bbs.huaweicloud.com/blogs/" + blogID
	}
	return result, nil
}

// trimFrameURLs 压缩 iframe URL 列表，避免报错信息过长。
// 只保留可辨识的片段（域名后第一段路径），够定位就行。
func trimFrameURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if u == "" || u == "about:blank" {
			continue
		}
		if len(u) > 90 {
			u = u[:90] + "..."
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return []string{"(无)"}
	}
	return out
}

// shotBlogFail 发文章失败时截图存档，便于事后定位。
// 截图失败不影响主流程 —— 它本身只是诊断辅助。
func shotBlogFail(page playwright.Page, tag string) {
	path := "/tmp/blog-fail-" + tag + ".png"
	_, _ = page.Screenshot(playwright.PageScreenshotOptions{
		Path: playwright.String(path),
	})
}
