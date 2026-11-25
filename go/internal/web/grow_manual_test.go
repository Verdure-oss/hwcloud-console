package web

import (
	"fmt"
	"testing"
)

// TestSummarizeStepsManual 验证「待人工」不被算成失败。
func TestSummarizeStepsManual(t *testing.T) {
	steps := []map[string]any{
		{"name": "提Issue", "ok": true, "msg": "已提交"},
		{"name": "Star Grape", "ok": false, "msg": "需要先绑定 GitHub",
			"detail": map[string]any{"need_bind": true, "need_manual": true}},
		{"name": "ICT创新赛", "ok": false, "msg": "需实名认证",
			"detail": map[string]any{"need_manual": true}},
	}
	res := summarizeSteps("周常", steps)
	fmt.Printf("RESULT=%v\n", res)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("待人工不应算失败，得到 ok=false: %v", res["msg"])
	}
	if got := res["msg"]; got != "周常：成功 1，待人工 2" {
		t.Fatalf("汇总文案不符: %v", got)
	}
}

// TestSummarizeStepsRealFailure 验证真正的失败仍然算失败。
func TestSummarizeStepsRealFailure(t *testing.T) {
	steps := []map[string]any{
		{"name": "提Issue", "ok": false, "msg": "HTTP 500"},
	}
	res := summarizeSteps("周常", steps)
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("真实失败必须 ok=false")
	}
}
