package web

import (
	"fmt"
	"hwcloud/internal/huawei"
	"os"
	"time"
)

// notifyBeanSession 是通知取积分所需的最小接口。
//
// 新站（grow-dataset）只需要 Points()；保留 BeansE 兼容旧测试/旧会话。
type notifyBeanSession interface {
	BeansE() (int, error)
}

// growNotifySession 只依赖新站接口。
type growNotifySession interface {
	Points() (int, error)
}

var newNotifySession = func(dir string) (notifyBeanSession, error) { return huawei.NewForAccount(dir) }

// newGrowNotifySession 默认用新站会话；测试可替换。
var newGrowNotifySession = func(dir string) (growNotifySession, error) {
	return huawei.NewGrowSessionForAccount(dir)
}

func resultFailed(result map[string]any) bool {
	if result == nil {
		return true
	}
	if e, ok := result["error"].(string); ok && e != "" {
		return true
	}
	if ok, exists := result["ok"].(bool); exists && !ok {
		return true
	}
	return false
}

func allBeansKnown(lines []beanLine) bool {
	for _, line := range lines {
		if !line.OK {
			return false
		}
	}
	return true
}

// NotifyCLIResult waits synchronously; a scheduled per-account invocation is
// suppressed so the enclosing script can send exactly one batch notification.
func NotifyCLIResult(name, account string, result map[string]any, elapsed time.Duration) {
	if !pushWorthyTask(name) {
		return
	}
	if os.Getenv("HWCLOUD_NOTIFY_SUPPRESS") == "1" {
		return
	}
	if err := NotifyCompletedForCLI(name, account, result, elapsed); err != nil {
		svcLog("[pushplus] 通知初始化失败")
	}
}

// NotifyCompletedForCLI only queries current balances and notifies. It never
// executes tasks or attempts login. Notification failures do not change business exit.
func NotifyCompletedForCLI(name, account string, result map[string]any, elapsed time.Duration) error {
	if !pushWorthyTask(name) {
		return fmt.Errorf("不支持通知的任务类型")
	}
	s, err := NewServer("")
	if err != nil {
		return fmt.Errorf("无法加载通知账号")
	}
	n := &jobNotify{Name: name, Label: taskLabel(name), Account: account, Result: result, Elapsed: elapsed}
	if resultFailed(result) {
		n.ErrMsg = "任务未全部成功，详情见任务日志"
	}
	s.notifyJobDone(n)
	return nil
}
