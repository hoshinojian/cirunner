// Package githubapi 以 gh CLI 子进程方式访问 GitHub Actions API：
// 排队 run、runner 状态、registration token 铸发、残留注册清理。
// 认证完全复用本机 gh 登录态（铸 token 需要 repo admin 权限）。
package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Client 是 gh 子进程封装。
type Client struct {
	GhBin string
	Run   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewClient 构造客户端。
func NewClient(ghBin string) *Client {
	c := &Client{GhBin: ghBin}
	c.Run = runGH
	return c
}

// Runner 是 GitHub 侧 runner 注册。
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// QueuedInfo 汇总排队 run。
type QueuedInfo struct {
	Count      int
	OldestAge  time.Duration // 相对调用时刻；Count==0 时为零值
	ObservedAt time.Time
}

type queuedJQ struct {
	N      int     `json:"n"`
	Oldest *string `json:"oldest"`
}

// QueuedRuns 返回排队中的 workflow run 数与最老年龄（P139 口径：判据只用 GitHub 侧信号）。
func (c *Client) QueuedRuns(ctx context.Context, repo string) (QueuedInfo, error) {
	out, err := c.Run(ctx, c.GhBin, "api",
		fmt.Sprintf("repos/%s/actions/runs?status=queued&per_page=100", repo),
		"--jq", `{n: .total_count, oldest: ([.workflow_runs[].created_at] | min)}`)
	if err != nil {
		return QueuedInfo{}, fmt.Errorf("gh api queued runs: %w", err)
	}
	var q queuedJQ
	if err := json.Unmarshal(out, &q); err != nil {
		return QueuedInfo{}, fmt.Errorf("解析 queued 输出: %w", err)
	}
	info := QueuedInfo{Count: q.N, ObservedAt: time.Now()}
	if q.Oldest != nil {
		created, err := time.Parse(time.RFC3339, *q.Oldest)
		if err != nil {
			return QueuedInfo{}, fmt.Errorf("解析排队时间 %q: %w", *q.Oldest, err)
		}
		info.OldestAge = info.ObservedAt.Sub(created)
	}
	return info, nil
}

// InProgressQueuedJobs 数 in_progress run 内仍排队（status=queued）的 job 数与
// 最老年龄。热补丁缺陷 #2：run 级 status 不可靠（P141）——run 一旦有 job 开跑
// 即变 in_progress，status=queued 过滤看不见其中剩余排队 job，需逐 run 拉 jobs 补数。
func (c *Client) InProgressQueuedJobs(ctx context.Context, repo string) (QueuedInfo, error) {
	out, err := c.Run(ctx, c.GhBin, "api",
		fmt.Sprintf("repos/%s/actions/runs?status=in_progress&per_page=100", repo),
		"--jq", `[.workflow_runs[].id]`)
	if err != nil {
		return QueuedInfo{}, fmt.Errorf("gh api in_progress runs: %w", err)
	}
	var runIDs []int64
	if err := json.Unmarshal(out, &runIDs); err != nil {
		return QueuedInfo{}, fmt.Errorf("解析 in_progress run 列表: %w", err)
	}
	info := QueuedInfo{ObservedAt: time.Now()}
	for _, id := range runIDs {
		out, err := c.Run(ctx, c.GhBin, "api",
			fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, id),
			"--jq", `{n: ([.jobs[] | select(.status == "queued")] | length), oldest: ([.jobs[] | select(.status == "queued") | .created_at] | min)}`)
		if err != nil {
			return QueuedInfo{}, fmt.Errorf("gh api run %d jobs: %w", id, err)
		}
		var q queuedJQ
		if err := json.Unmarshal(out, &q); err != nil {
			return QueuedInfo{}, fmt.Errorf("解析 run %d jobs 输出: %w", id, err)
		}
		info.Count += q.N
		if q.Oldest != nil {
			created, err := time.Parse(time.RFC3339, *q.Oldest)
			if err != nil {
				return QueuedInfo{}, fmt.Errorf("解析排队时间 %q: %w", *q.Oldest, err)
			}
			if age := info.ObservedAt.Sub(created); age > info.OldestAge {
				info.OldestAge = age
			}
		}
	}
	return info, nil
}

// MergeQueued 合并两路排队观察（queued run 与 in_progress run 内 queued job）：
// 数量求和，最老排队年龄按绝对创建时刻比较，观察基准取较新一路。
func MergeQueued(a, b QueuedInfo) QueuedInfo {
	out := QueuedInfo{Count: a.Count + b.Count, ObservedAt: a.ObservedAt}
	if b.ObservedAt.After(out.ObservedAt) {
		out.ObservedAt = b.ObservedAt
	}
	oldest := oldestAbs(a)
	if abs := oldestAbs(b); oldest.IsZero() || (!abs.IsZero() && abs.Before(oldest)) {
		oldest = abs
	}
	if !oldest.IsZero() {
		out.OldestAge = out.ObservedAt.Sub(oldest)
	}
	return out
}

func oldestAbs(q QueuedInfo) time.Time {
	if q.Count == 0 || q.OldestAge <= 0 {
		return time.Time{}
	}
	return q.ObservedAt.Add(-q.OldestAge)
}

// Runners 返回仓内全部 self-hosted runner 注册（含外部/手工机的——前缀过滤由 decide 做）。
func (c *Client) Runners(ctx context.Context, repo string) ([]Runner, error) {
	out, err := c.Run(ctx, c.GhBin, "api",
		fmt.Sprintf("repos/%s/actions/runners", repo),
		"--jq", `.runners | map({id, name, status, busy})`)
	if err != nil {
		return nil, fmt.Errorf("gh api runners: %w", err)
	}
	var rs []Runner
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, fmt.Errorf("解析 runners 输出: %w", err)
	}
	return rs, nil
}

// MintToken 铸 registration token（1h 时效，建机瞬间现铸现用）。
func (c *Client) MintToken(ctx context.Context, repo string) (string, error) {
	out, err := c.Run(ctx, c.GhBin, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/actions/runners/registration-token", repo),
		"--jq", ".token")
	if err != nil {
		return "", fmt.Errorf("gh api registration-token: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if len(tok) < 16 {
		return "", fmt.Errorf("registration token 异常（长度 %d）", len(tok))
	}
	return tok, nil
}

// RemoveRunner 清理 runner 注册（删机后清 offline 残留）。
func (c *Client) RemoveRunner(ctx context.Context, repo string, id int64) error {
	if _, err := c.Run(ctx, c.GhBin, "api", "-X", "DELETE",
		fmt.Sprintf("repos/%s/actions/runners/%d", repo, id)); err != nil {
		return fmt.Errorf("gh api remove runner %d: %w", id, err)
	}
	return nil
}
