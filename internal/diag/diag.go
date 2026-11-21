// Package diag 采集诊断材料（REDESIGN §5.4）：health 全量 + 各段文本。
// 单段失败不影响其他段（该段写 {"error": "..."}）；总输出 ≤1MB。
package diag

import (
	"context"

	"github.com/hoshinojian/cirunner/internal/health"
	"github.com/hoshinojian/cirunner/internal/sys"
)

// Section 是一段文本材料；失败时 Err 非空。
type Section struct {
	Text string `json:"text,omitempty"`
	Err  string `json:"error,omitempty"`
}

// Report 是 diag 的输出。
type Report struct {
	Health   health.Report      `json:"health"`
	Sections map[string]Section `json:"sections"`
}

// maxTotalBytes 是总输出上限（§5.4：≤1MB）。
const maxTotalBytes = 1 << 20

// Collect 采集全部材料。lines 是每段行数（--lines）。
func Collect(ctx context.Context, h *sys.Host, lines int) Report {
	r := Report{Sections: map[string]Section{}}
	r.Health = health.Collect(ctx, h)

	collect := func(key string, f func() (string, error)) {
		txt, err := f()
		if err != nil {
			r.Sections[key] = Section{Err: err.Error()}
			return
		}
		r.Sections[key] = Section{Text: txt}
	}

	collect("cloud-init-output.log", func() (string, error) {
		return h.ReadFileTail(ctx, "/var/log/cloud-init-output.log", lines)
	})
	collect("runner-journal", func() (string, error) {
		return h.JournalTail(ctx, "actions.runner.*", lines)
	})
	// P139 Listener 挂起的关键证据在 _diag 日志
	collect("runner-diag-log", func() (string, error) {
		return h.LatestDiagLog(ctx, "Runner_*.log", lines)
	})
	collect("runner-worker-log", func() (string, error) {
		return h.LatestDiagLog(ctx, "Worker_*.log", lines)
	})
	collect("docker-journal", func() (string, error) {
		return h.JournalTail(ctx, "docker", lines)
	})
	collect("docker-ps", func() (string, error) {
		return h.DockerPS(ctx)
	})
	collect("df-h", func() (string, error) {
		out, err := h.Cmd.Run(ctx, "df", "-h")
		if err != nil {
			return "", err
		}
		return string(out), nil
	})
	collect("free-m", func() (string, error) {
		out, err := h.Cmd.Run(ctx, "free", "-m")
		if err != nil {
			return "", err
		}
		return string(out), nil
	})
	collect("dmesg-tail", func() (string, error) {
		return h.DmesgTail(ctx, lines)
	})
	// unattended-upgrades 坑的证据
	collect("apt-20auto-upgrades", func() (string, error) {
		return h.AptAutoUpgrades(ctx)
	})
	collect("apt-history", func() (string, error) {
		return h.AptHistoryTail(ctx, lines)
	})

	truncateSections(r.Sections)
	return r
}

// truncateSections 按总上限截断（保头去尾：优先保留 error 信息）。
func truncateSections(sections map[string]Section) {
	total := 0
	for _, v := range sections {
		total += len(v.Text) + len(v.Err)
	}
	if total <= maxTotalBytes {
		return
	}
	// 超限：等比砍每段，保 1KB 头部（错误信息通常在头尾）
	budget := maxTotalBytes
	for k, v := range sections {
		share := maxTotalBytes / len(sections)
		if share < 1024 {
			share = 1024
		}
		if len(v.Text) > share {
			keep := v.Text[:share/2] + "\n...[diag 截断]...\n" + v.Text[len(v.Text)-share/2:]
			sections[k] = Section{Text: keep, Err: v.Err}
		}
		budget -= share
		if budget <= 0 {
			break
		}
	}
}
