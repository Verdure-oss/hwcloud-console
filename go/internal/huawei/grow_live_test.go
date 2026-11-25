package huawei

import (
	"fmt"
	"os"
	"testing"
)

// TestGrowLiveReadOnly 只读验证真实账号（需要 /opt/hwcloud-demo 环境）。
// 默认跳过；设置 HWCLOUD_LIVE=1 才执行。
func TestGrowLiveReadOnly(t *testing.T) {
	if osGetenv("HWCLOUD_LIVE") != "1" {
		t.Skip("set HWCLOUD_LIVE=1 to run")
	}
	accounts := []string{"13005999962", "13377298897", "15730229941", "15730639941"}
	for _, acct := range accounts {
		dir := "/opt/hwcloud-demo/accounts/" + acct + "/state"
		s, err := NewGrowSessionForAccount(dir)
		if err != nil {
			t.Fatalf("%s: %v", acct, err)
		}
		u, err := s.UserInfo()
		if err != nil {
			t.Fatalf("%s user info: %v", acct, err)
		}
		fmt.Printf("LIVE %s user=%s points=%d signed=%v\n", acct, u.DomainName, u.TotalPoints, u.Signed)
		tasks, err := s.Tasks()
		if err != nil {
			t.Fatalf("%s tasks: %v", acct, err)
		}
		var todo []string
		for _, task := range tasks {
			if !task.Finished() {
				todo = append(todo, task.TaskTag)
			}
		}
		fmt.Printf("LIVE %s tasks=%d todo=%v\n", acct, len(tasks), todo)
	}
}

func osGetenv(k string) string {
	return os.Getenv(k)
}
