// Package act 是机器侧白名单动作（REDESIGN §5.4）：restart-runner /
// stop-runner / clean-work。未知动作由 CLI 层拒（退出码 2）。
package act

import (
	"context"
	"fmt"
	"time"

	"github.com/hoshinojian/cirunner/internal/sys"
)

// Result 是动作输出（JSON 形态 {"action","ok","detail"}）。
type Result struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// listenerWait 是 restart-runner 后等待 Listener 出现的上限（§5.4：30s；
// var 以便测试注入缩短）。
var listenerWait = 30 * time.Second

// RestartRunner 重启全部 runner 实例并等待 Listener。
func RestartRunner(ctx context.Context, h *sys.Host) Result {
	units, err := h.RunnerUnits(ctx)
	if err != nil {
		return Result{Action: "restart-runner", OK: false, Detail: fmt.Sprintf("列 unit 失败: %v", err)}
	}
	if len(units) == 0 {
		return Result{Action: "restart-runner", OK: false, Detail: "无 actions.runner.* unit"}
	}
	if err := h.Systemctl(ctx, append([]string{"restart"}, units...)...); err != nil {
		return Result{Action: "restart-runner", OK: false, Detail: fmt.Sprintf("restart 失败: %v", err)}
	}
	// rpd=2 时要等 Listener 数达到 unit 数（复审修正：出现一个就报成功会漏判
	// 另一实例挂起）
	deadline := time.Now().Add(listenerWait)
	for time.Now().Before(deadline) {
		if h.PgrepCount(ctx, "Runner.Listener") >= len(units) {
			return Result{Action: "restart-runner", OK: true,
				Detail: fmt.Sprintf("已重启 %d 个实例，Listener 全部出现", len(units))}
		}
		time.Sleep(time.Second)
	}
	return Result{Action: "restart-runner", OK: false,
		Detail: fmt.Sprintf("已重启但 30s 内 Listener 数未达 %d（P139 式挂起，建议 diag）", len(units))}
}

// StopRunner 停止全部 runner 实例。
func StopRunner(ctx context.Context, h *sys.Host) Result {
	units, err := h.RunnerUnits(ctx)
	if err != nil {
		return Result{Action: "stop-runner", OK: false, Detail: fmt.Sprintf("列 unit 失败: %v", err)}
	}
	if len(units) == 0 {
		return Result{Action: "stop-runner", OK: false, Detail: "无 actions.runner.* unit"}
	}
	if err := h.Systemctl(ctx, append([]string{"stop"}, units...)...); err != nil {
		return Result{Action: "stop-runner", OK: false, Detail: fmt.Sprintf("stop 失败: %v", err)}
	}
	return Result{Action: "stop-runner", OK: true, Detail: fmt.Sprintf("已停止 %d 个实例", len(units))}
}

// CleanWork 清 _work（仅在 runner 服务停止状态下允许，防删正在跑的 job）。
func CleanWork(ctx context.Context, h *sys.Host) Result {
	units, err := h.RunnerUnits(ctx)
	if err != nil {
		return Result{Action: "clean-work", OK: false, Detail: fmt.Sprintf("列 unit 失败: %v", err)}
	}
	for _, u := range units {
		state, err := h.UnitState(ctx, u)
		if err != nil {
			return Result{Action: "clean-work", OK: false, Detail: fmt.Sprintf("查 %s 状态失败: %v", u, err)}
		}
		if state == "active" {
			return Result{Action: "clean-work", OK: false,
				Detail: fmt.Sprintf("%s 仍在 active（先 stop-runner）", u)}
		}
	}
	if _, err := h.Cmd.Run(ctx, "sh", "-c", "rm -rf /opt/actions-runner*/_work/*"); err != nil {
		return Result{Action: "clean-work", OK: false, Detail: fmt.Sprintf("清理失败: %v", err)}
	}
	return Result{Action: "clean-work", OK: true, Detail: "已清 /opt/actions-runner*/_work/*"}
}
