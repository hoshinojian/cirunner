// Package health 汇总机器健康检查（REDESIGN §5.4）。
package health

import (
	"context"

	"github.com/hoshinojian/cirunner/internal/sys"
)

// RunnerService 是一个 runner 服务unit的状态。
type RunnerService struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
}

// Process 是进程探测结果。
type Process struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// Checks 是全部检查项（JSON 形态对齐 §5.4）。
type Checks struct {
	CloudInit struct {
		Status string `json:"status"` // done|running|error|unknown
		Stage  string `json:"stage"`  // /var/lib/cirunner/stage
	} `json:"cloud_init"`
	RunnerServices  []RunnerService `json:"runner_services"`
	ListenerProcess Process         `json:"listener_process"`
	WorkerProcess   Process         `json:"worker_process"`
	Docker          struct {
		Active bool `json:"active"`
	} `json:"docker"`
	DiskRootFreePct float64 `json:"disk_root_free_pct"`
	MemAvailableMB  int     `json:"mem_available_mb"`
	Load1           float64 `json:"load1"`
	RunnerVersion   string  `json:"runner_version"`
	UptimeSec       int     `json:"uptime_sec"`
}

// Report 是 health 的输出。
type Report struct {
	OK     bool   `json:"ok"`
	Checks Checks `json:"checks"`
}

// diskFreeFloorPct 是 ok=false 的磁盘阈值（§5.4：<10%）。
const diskFreeFloorPct = 10.0

// Collect 采集全部检查。单项失败不中断（该字段零值，ok 判定自然体现）。
func Collect(ctx context.Context, h *sys.Host) Report {
	var r Report
	c := Checks{}

	c.CloudInit.Status = h.CloudInitStatus(ctx)
	c.CloudInit.Stage = h.StageMarker(ctx)

	if units, err := h.RunnerUnits(ctx); err == nil {
		for _, u := range units {
			state, err := h.UnitState(ctx, u)
			if err != nil {
				state = "unknown"
			}
			c.RunnerServices = append(c.RunnerServices, RunnerService{Unit: u, Active: state})
		}
	}

	if ok, pid, _ := h.PgrepRunning(ctx, "Runner.Listener"); ok {
		c.ListenerProcess = Process{Running: true, PID: pid}
	}
	if ok, pid, _ := h.PgrepRunning(ctx, "Runner.Worker"); ok {
		c.WorkerProcess = Process{Running: true, PID: pid}
	}

	c.Docker.Active = h.DockerActive(ctx)
	c.DiskRootFreePct, _ = h.DiskRootFreePct(ctx)
	c.MemAvailableMB, _ = h.MemAvailableMB(ctx)
	c.Load1, _ = h.Load1(ctx)
	c.RunnerVersion = h.RunnerVersion(ctx)
	c.UptimeSec, _ = h.UptimeSec(ctx)

	r.Checks = c
	r.OK = judge(c)
	return r
}

// judge 按 §5.4：cloud-init error、runner 服务非 active、Listener 不在、
// docker 非 active、磁盘 <10% 任一成立即 ok=false。
func judge(c Checks) bool {
	if c.CloudInit.Status == "error" {
		return false
	}
	if len(c.RunnerServices) == 0 {
		return false
	}
	for _, s := range c.RunnerServices {
		if s.Active != "active" {
			return false
		}
	}
	if !c.ListenerProcess.Running {
		return false
	}
	if !c.Docker.Active {
		return false
	}
	if c.DiskRootFreePct > 0 && c.DiskRootFreePct < diskFreeFloorPct {
		return false
	}
	return true
}
