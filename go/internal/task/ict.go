package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/config"
)

// ICT 大赛创新赛报名（新任务 +350，一次性）。
//
// 新任务说明（2026-11-23）：登录 ICT 大赛平台，报名创新赛，绑定华为云账号，
// 完成实名认证，查看报名信息，并开通华为云码道。
//
// 实测结论（2026-11-23）：
//   - 页面是 gallery.developer.huaweicloud.com 的「华为ICT大赛-创新赛 报名积分领取」，
//     三步：01 登录华为云账号 → 02 实名认证 → 03 授权并领取积分。
//   - 该页面用**独立的 gallery 成员会话**（__Host-bp_session + x-csrf-token），
//     华为云 koocdp_sid/Cookie 直接打 /open-web-user/... 会 401
//     GALLERY.AUTH.UNAUTHORIZED，不能跨体系复用。
//   - 报名接口 /open-web-user/v1/gallery/competition/registration-task 与
//     /registration?variant=innovation 都需要上述成员会话。
//
// 所以这里只做只读探测：能打开页面就如实报告「会话状态 + 当前步骤」；
// 需要人工完成的（gallery 登录、实名、授权领取）明确列出，不返回假成功。
const ictInnovationURL = "https://gallery.developer.huaweicloud.com/gallery/competition?type=innovation"

// ICTInnovationStatus 只读探测 ICT 创新赛报名入口。
func ICTInnovationStatus(headless bool) (map[string]any, error) {
	out := map[string]any{"url": ictInnovationURL, "ok": false}
	acct := config.AccountID()
	profile, port := galleryProfileFor(acct)
	if err := ensureGalleryBrowser(profile, port); err != nil {
		out["msg"] = "浏览器启动失败: " + err.Error()
		return out, nil
	}
	defer closeGalleryBrowser(port)

	pw, err := playwright.Run()
	if err != nil {
		out["msg"] = "Playwright 启动失败: " + err.Error()
		return out, nil
	}
	defer pw.Stop()
	br, err := pw.Chromium.ConnectOverCDP("http://127.0.0.1:" + port)
	if err != nil {
		out["msg"] = "CDP 连接失败: " + err.Error()
		return out, nil
	}
	defer br.Close()
	if len(br.Contexts()) == 0 {
		out["msg"] = "浏览器没有可用上下文"
		return out, nil
	}
	ctx := br.Contexts()[0]
	if err := seedHuaweiCookies(ctx); err != nil {
		out["msg"] = "注入华为云会话失败: " + err.Error()
		return out, nil
	}
	page, err := ctx.NewPage()
	if err != nil {
		out["msg"] = "新建页面失败: " + err.Error()
		return out, nil
	}
	defer page.Close()
	if _, err := page.Goto(ictInnovationURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(45000),
	}); err != nil {
		out["msg"] = "打开 ICT 赛事页失败: " + err.Error()
		return out, nil
	}
	time.Sleep(8 * time.Second)
	if title, err := page.Title(); err == nil {
		out["page_title"] = title
	}
	txt, err := page.InnerText("body")
	if err != nil {
		out["msg"] = "读取页面失败: " + err.Error()
		return out, nil
	}
	s := strings.TrimSpace(txt)
	if r := []rune(s); len(r) > 500 {
		s = string(r[:500]) + "…"
	}
	out["page_text"] = s
	// 判据用正向标记，不看「是否出现登录二字」：
	// 页面同时有「登录华为云账号」标题和「✓ 已登录（hid_xxx）」，后者才是真相。
	switch {
	case strings.Contains(txt, "已登录"):
		out["logged_in"] = true
		switch {
		case strings.Contains(txt, "实名认证") && strings.Contains(txt, "授权并领取积分"):
			out["step_hint"] = "第2步：需完成实名认证并授权领取积分"
			out["need_manual"] = true
			out["msg"] = "ICT 页面已登录；实名认证 / 授权领取需人工在页面完成"
		default:
			out["step_hint"] = "登录已就绪"
			// 报名/提交同样只能人工，这一步自动化到不了终点。
			out["need_manual"] = true
			out["msg"] = "ICT 页面已登录；报名需在页面继续操作"
		}
	case strings.Contains(txt, "立即登录"):
		out["logged_in"] = false
		out["need_login"] = true
		out["need_manual"] = true
		out["msg"] = "ICT 页面需要先用 gallery 成员会话登录华为云（与成长中心会话独立）"
	default:
		out["need_manual"] = true
		out["msg"] = "ICT 页面状态无法识别，已记录页面文本"
	}
	return out, nil
}

var _ = fmt.Sprintf
