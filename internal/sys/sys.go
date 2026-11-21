// Package sys 封装机器侧子进程调用（systemctl/journalctl/df/free/pgrep/
// docker/ss），全部经 Cmd 接口（可 fake，REDESIGN §5.2）。
package sys

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Cmd 是一次命令执行能力（真实实现 Exec；测试用 fake）。
type Cmd interface {
	// Run 执行命令返回 stdout；非零退出码返回 err（err 含 stderr 摘要由实现负责）。
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Exec 是真实子进程实现。
type Exec struct{}

// Run 实现 Cmd。
func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%s: %w: %s", name, err, truncate(ee.Stderr, 200))
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// Host 是机器侧探查集合（health/diag/act 共用）。
type Host struct{ Cmd Cmd }

// NewHost 构造（真实子进程）。
func NewHost() *Host { return &Host{Cmd: Exec{}} }

// RunnerUnits 列出 actions.runner.* unit（含 inactive 的：--all）。
func (h *Host) RunnerUnits(ctx context.Context) ([]string, error) {
	out, err := h.Cmd.Run(ctx, "systemctl", "list-units", "actions.runner.*", "--all",
		"--no-legend", "--plain", "--no-pager")
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		unit := strings.Fields(line)[0]
		if strings.HasPrefix(unit, "actions.runner.") {
			units = append(units, unit)
		}
	}
	return units, nil
}

// UnitState 返回 unit 的 active 状态（active/inactive/failed…）。
func (h *Host) UnitState(ctx context.Context, unit string) (string, error) {
	out, err := h.Cmd.Run(ctx, "systemctl", "show", unit, "--property", "ActiveState", "--value")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Systemctl 执行任意 systemctl 动作。
func (h *Host) Systemctl(ctx context.Context, args ...string) error {
	_, err := h.Cmd.Run(ctx, "systemctl", args...)
	return err
}

// JournalTail 取 unit journal 末尾 n 行。
func (h *Host) JournalTail(ctx context.Context, unit string, n int) (string, error) {
	out, err := h.Cmd.Run(ctx, "journalctl", "-u", unit, "--no-pager", "-n", strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// PgrepRunning 判断进程模式是否存在（pgrep -f）。
func (h *Host) PgrepRunning(ctx context.Context, pattern string) (bool, int, error) {
	out, err := h.Cmd.Run(ctx, "pgrep", "-f", pattern)
	if err != nil || len(out) == 0 {
		return false, 0, nil // pgrep 无匹配时退出码非零/输出为空：视为不存在
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
	if convErr != nil {
		return true, 0, nil
	}
	return true, pid, nil
}

// DiskRootFreePct 返回根分区可用百分比。
func (h *Host) DiskRootFreePct(ctx context.Context) (float64, error) {
	out, err := h.Cmd.Run(ctx, "df", "-P", "/")
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df 输出行数异常")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 6 {
		return 0, fmt.Errorf("df 字段数异常")
	}
	avail, err1 := strconv.ParseFloat(fields[3], 64)
	total, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return 0, fmt.Errorf("df 数值异常")
	}
	return avail * 100 / total, nil
}

// MemAvailableMB 返回可用内存（MB）。
func (h *Host) MemAvailableMB(ctx context.Context) (int, error) {
	out, err := h.Cmd.Run(ctx, "free", "-m")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Mem:") {
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				return strconv.Atoi(fields[6])
			}
		}
	}
	return 0, fmt.Errorf("free 输出无 Mem 行")
}

// Load1 返回 1 分钟负载。
func (h *Host) Load1(ctx context.Context) (float64, error) {
	out, err := h.Cmd.Run(ctx, "cut", "-d", " ", "-f1", "/proc/loadavg")
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

// UptimeSec 返回开机秒数。
func (h *Host) UptimeSec(ctx context.Context) (int, error) {
	out, err := h.Cmd.Run(ctx, "cut", "-d", ".", "-f1", "/proc/uptime")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// RunnerVersion 用 config.sh --version 取版本（沙箱实录 2026-11-21：.runner
// 文件里没有 version 字段；config.sh --version 输出如 "2.338.2"）。
func (h *Host) RunnerVersion(ctx context.Context) string {
	out, err := h.Cmd.Run(ctx, "sh", "-c",
		`for d in /opt/actions-runner /opt/actions-runner-a /opt/actions-runner-b; do [ -x $d/config.sh ] && sudo -u runner $d/config.sh --version 2>/dev/null && return 0; done; true`)
	if err != nil || len(out) == 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// StageMarker 读 /var/lib/cirunner/stage（无则空）。
func (h *Host) StageMarker(ctx context.Context) string {
	out, err := h.Cmd.Run(ctx, "cat", "/var/lib/cirunner/stage")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CloudInitDone 判定 cloud-init 是否完成（cloud-init status 稳态）。
// 返回 status（done/running/error/unknown）。
func (h *Host) CloudInitStatus(ctx context.Context) string {
	out, err := h.Cmd.Run(ctx, "cloud-init", "status")
	s := strings.TrimSpace(string(out))
	if err != nil {
		// cloud-init status 在异常时以非零退出并输出 status: error
		if strings.Contains(s, "error") {
			return "error"
		}
		return "unknown"
	}
	switch {
	case strings.Contains(s, "done"):
		return "done"
	case strings.Contains(s, "running"):
		return "running"
	case strings.Contains(s, "error"):
		return "error"
	}
	return "unknown"
}

// ReadFileTail 读文件末尾 n 行（文件大时只取尾部 1MB 再截行）。
func (h *Host) ReadFileTail(ctx context.Context, path string, n int) (string, error) {
	out, err := h.Cmd.Run(ctx, "tail", "-n", strconv.Itoa(n), path)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// FileExists 判定文件存在。
func (h *Host) FileExists(ctx context.Context, path string) bool {
	_, err := h.Cmd.Run(ctx, "test", "-f", path)
	return err == nil
}

// DockerActive 判定 docker 服务 active。
func (h *Host) DockerActive(ctx context.Context) bool {
	state, err := h.UnitState(ctx, "docker")
	return err == nil && state == "active"
}

// DockerPS 返回 docker ps -a 输出。
func (h *Host) DockerPS(ctx context.Context) (string, error) {
	out, err := h.Cmd.Run(ctx, "sh", "-c", "docker ps -a --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}' 2>&1 | head -30")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// DmesgTail 返回 dmesg 末尾（OOM 证据）。
func (h *Host) DmesgTail(ctx context.Context, n int) (string, error) {
	out, err := h.Cmd.Run(ctx, "sh", "-c", "dmesg 2>/dev/null | tail -n "+strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// LatestDiagLog 取匹配模式的最新日志文件末尾 n 行（P139 Listener 挂起证据
// 在 _diag/Runner_*.log / Worker_*.log）。
func (h *Host) LatestDiagLog(ctx context.Context, pattern string, n int) (string, error) {
	out, err := h.Cmd.Run(ctx, "sh", "-c",
		`ls -t /opt/actions-runner*/_diag/`+pattern+` 2>/dev/null | head -1`)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("无 %s 日志", pattern)
	}
	path := strings.TrimSpace(string(out))
	return h.ReadFileTail(ctx, path, n)
}

// AptHistoryTail 取 apt history 末尾（unattended-upgrades 坑证据）。
func (h *Host) AptHistoryTail(ctx context.Context, n int) (string, error) {
	return h.ReadFileTail(ctx, "/var/log/apt/history.log", n)
}

// AptAutoUpgrades 读 /etc/apt/apt.conf.d/20auto-upgrades。
func (h *Host) AptAutoUpgrades(ctx context.Context) (string, error) {
	return h.ReadFileTail(ctx, "/etc/apt/apt.conf.d/20auto-upgrades", 10)
}
