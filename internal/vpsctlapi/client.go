// Package vpsctlapi 以子进程方式驱动 vpsctl CLI（建/列/删），解析其 JSON 输出。
// vpsctl 全部库代码在 internal/ 下（Go 禁止跨仓 import），子进程是唯一依赖形态；
// 本包不改 vpsctl 任何行为，参数与输出契约见 vpsctl 仓 cmd/vpsctl 与 internal/fleet。
package vpsctlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Client 是 vpsctl 子进程封装。
type Client struct {
	Bin          string // vpsctl 二进制绝对路径
	AccountsFile string // 可选 --accounts（vpsctl 账号配置）
	Run          func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewClient 构造客户端；Run 可在测试中替换。
func NewClient(bin, accountsFile string) *Client {
	c := &Client{Bin: bin, AccountsFile: accountsFile}
	c.Run = execOutput
	return c
}

// Droplet 是 vpsctl JSON 输出里的节点（字段名与 vpsctl ServerJSON 对齐）。
type Droplet struct {
	Account    string    `json:"account"`
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Region     string    `json:"region"`
	Size       string    `json:"size"`
	IPv4Public string    `json:"ipv4_public"`
	Tags       []string  `json:"tags"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateOpts 是建机参数（单台；账号固定 --only，防全账号误建）。
type CreateOpts struct {
	Account      string
	Region       string
	Size         string
	Image        string
	NamePrefix   string
	StartIndex   int // 显式指定，保证 droplet 名与预渲染的 runner 名一致
	Tags         []string
	UserDataFile string // cloud-init 文件路径（vpsctl --user-data 收路径）
	Wait         time.Duration
}

type createResult struct {
	Batch   string        `json:"batch"`
	Created []Droplet     `json:"created"`
	Errors  []createError `json:"errors"`
}

type createError struct {
	Account string `json:"account"`
	Name    string `json:"name"`
	Error   string `json:"error"`
}

type listResult struct {
	Droplets []Droplet   `json:"droplets"`
	Errors   []listError `json:"errors"`
}

type listError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

type opResult struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
}

func (c *Client) baseArgs(sub string) []string {
	args := []string{sub}
	if c.AccountsFile != "" {
		args = append(args, "-accounts", c.AccountsFile)
	}
	return args
}

// Create 建一台机并等待 active+公网 IP（--wait 语义：不等 cloud-init 完成；
// runner online 由编排器从 GitHub 侧确认）。
func (c *Client) Create(ctx context.Context, o CreateOpts) (Droplet, error) {
	if o.UserDataFile == "" {
		return Droplet{}, fmt.Errorf("UserDataFile 必填（cloud-init 文件路径）")
	}
	args := append(c.baseArgs("create"),
		"-only", o.Account,
		"-region", o.Region,
		"-size", o.Size,
		"-image", o.Image,
		"-name-prefix", o.NamePrefix,
		"-count", "1",
		"-tags", strings.Join(o.Tags, ","),
		"-user-data", o.UserDataFile,
	)
	if o.StartIndex > 0 {
		args = append(args, "-start-index", fmt.Sprint(o.StartIndex))
	}
	if o.Wait > 0 {
		args = append(args, "-wait", o.Wait.String())
	}
	out, err := c.Run(ctx, c.Bin, args...)
	if err != nil && len(out) == 0 {
		return Droplet{}, fmt.Errorf("vpsctl create: %w", err)
	}
	var res createResult
	if perr := json.Unmarshal(out, &res); perr != nil {
		return Droplet{}, fmt.Errorf("vpsctl create 输出解析失败: %v (stderr 见上)", perr)
	}
	if len(res.Errors) > 0 {
		return Droplet{}, fmt.Errorf("vpsctl create 部分失败: %+v", res.Errors)
	}
	if len(res.Created) != 1 {
		return Droplet{}, fmt.Errorf("vpsctl create 期望 1 台，实得 %d 台", len(res.Created))
	}
	d := res.Created[0]
	if d.IPv4Public == "" {
		return d, fmt.Errorf("节点已创建（%s id=%s）但未就绪（无公网 IP），请用 vpsctl list --tag 查看并手动处置", d.Name, d.ID)
	}
	return d, nil
}

// ListByTag 按 DO tag 精确过滤列机。account 非空时透传 -only（同 token 多账号
// 会让同一台 droplet 在每个账号名下重复出现，必须钉死账号）。
func (c *Client) ListByTag(ctx context.Context, tag, account string) ([]Droplet, error) {
	args := append(c.baseArgs("list"), "-tag", tag)
	if account != "" {
		args = append(args, "-only", account)
	}
	out, err := c.Run(ctx, c.Bin, args...)
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("vpsctl list: %w", err)
	}
	var res listResult
	if perr := json.Unmarshal(out, &res); perr != nil {
		return nil, fmt.Errorf("vpsctl list 输出解析失败: %v", perr)
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("vpsctl list 部分账号失败: %+v", res.Errors)
	}
	return res.Droplets, nil
}

// Delete 非交互精确单删（vpsctl 安全闸：--ids account/id --confirm 1）。
func (c *Client) Delete(ctx context.Context, account string, id string) error {
	args := append(c.baseArgs("delete"),
		"-ids", account+"/"+id,
		"-confirm", "1",
	)
	out, err := c.Run(ctx, c.Bin, args...)
	if err != nil && len(out) == 0 {
		return fmt.Errorf("vpsctl delete: %w", err)
	}
	var res []opResult
	if perr := json.Unmarshal(out, &res); perr != nil {
		return fmt.Errorf("vpsctl delete 输出解析失败: %v", perr)
	}
	for _, op := range res {
		if !op.OK {
			return fmt.Errorf("vpsctl delete %s/%s 失败: %s", op.Account, op.ID, op.Error)
		}
	}
	return nil
}
