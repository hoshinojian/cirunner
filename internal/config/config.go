// Package config 加载并校验 cirunner 的 JSON 配置。
// 风格对齐 vpsctl 的 accounts.json（stdlib encoding/json，零三方依赖）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config 是全局配置。
type Config struct {
	VPSctlBin        string `json:"vpsctl_bin"`          // vpsctl 二进制绝对路径（显式锚定，不猜 PATH）
	AccountsFile     string `json:"accounts_file"`       // 可选，透传 vpsctl --accounts
	GhBin            string `json:"gh_bin"`              // gh 二进制，默认 "gh"
	SSHPublicKeyFile string `json:"ssh_public_key_file"` // cloud-init ssh_authorized_keys 来源（~ 可）
	StateDir         string `json:"state_dir"`           // 守护状态持久化目录，默认 ~/.local/state/cirunner
	PollIntervalSec  int    `json:"poll_interval_sec"`   // 采样间隔，默认 60
	IdleTimeoutMin   int    `json:"idle_timeout_min"`    // 空闲回收阈值（分钟），默认 20
	MaxDroplets      int    `json:"max_droplets"`        // 全局 droplet 上限，默认 2
	Repos            []Repo `json:"repos"`
}

// Repo 是单个租户仓的配置。
type Repo struct {
	Repo       string   `json:"repo"`        // "owner/name"
	Account    string   `json:"account"`     // vpsctl 账号名，透传 --only（必填，2026-11-13 全账号误建教训）
	Region     string   `json:"region"`      // DO region slug
	Size       string   `json:"size"`        // DO size slug（跑 -race 时敏测试须专用核 c 系，见 nms runbook）
	Image      string   `json:"image"`       // DO image slug
	NamePrefix string   `json:"name_prefix"` // droplet/runner 名前缀，默认 "cirunner"
	Labels     []string `json:"labels"`      // runner 注册 labels，默认 ["ci"]；须与 workflow runs-on 匹配
	Tags       []string `json:"tags"`        // 附加 DO tags（cirunner 与 repo:<slug> 总会自动带上）
	MaxRunners int      `json:"max_runners"` // 每仓 droplet 上限，默认 1
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var labelRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Load 按优先级读取配置：显式 path > $CIRUNNER_CONFIG > ~/.config/cirunner/cirunner.json，
// 应用默认值并校验。
func Load(path string) (*Config, error) {
	if path == "" {
		if env := os.Getenv("CIRUNNER_CONFIG"); env != "" {
			path = env
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, ".config", "cirunner", "cirunner.json")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.expandHome(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.GhBin == "" {
		c.GhBin = "gh"
	}
	if c.PollIntervalSec == 0 {
		c.PollIntervalSec = 60
	}
	if c.IdleTimeoutMin == 0 {
		c.IdleTimeoutMin = 20
	}
	if c.MaxDroplets == 0 {
		c.MaxDroplets = 2
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.NamePrefix == "" {
			r.NamePrefix = "cirunner"
		}
		if len(r.Labels) == 0 {
			r.Labels = []string{"ci"}
		}
		if r.MaxRunners == 0 {
			r.MaxRunners = 1
		}
	}
}

func (c *Config) expandHome() error {
	var err error
	expand := func(s *string) {
		if err == nil && *s != "" {
			*s, err = expandTilde(*s)
		}
	}
	expand(&c.VPSctlBin)
	expand(&c.AccountsFile)
	expand(&c.SSHPublicKeyFile)
	expand(&c.StateDir)
	return err
}

func expandTilde(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

func (c *Config) validate() error {
	if c.VPSctlBin == "" {
		return fmt.Errorf("vpsctl_bin 必填（显式锚定 vpsctl 二进制路径，vpsctl 无版本机制，便于核对 commit）")
	}
	if len(c.Repos) == 0 {
		return fmt.Errorf("repos 至少配置一个租户仓")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		what := fmt.Sprintf("repos[%d]", i)
		if !repoRe.MatchString(r.Repo) {
			return fmt.Errorf("%s.repo %q 须为 owner/name", what, r.Repo)
		}
		if r.Account == "" {
			return fmt.Errorf("%s(%s).account 必填：透传 vpsctl --only，防全账号误建（2026-11-13 教训）", what, r.Repo)
		}
		if r.Region == "" || r.Size == "" || r.Image == "" {
			return fmt.Errorf("%s(%s).region/size/image 必填", what, r.Repo)
		}
		if !labelRe.MatchString(r.NamePrefix) {
			return fmt.Errorf("%s.name_prefix %q 含非法字符", what, r.NamePrefix)
		}
		for _, l := range r.Labels {
			if !labelRe.MatchString(l) {
				return fmt.Errorf("%s.labels 含非法项 %q（label 会写入注册命令）", what, l)
			}
		}
		if seen[r.Repo] {
			return fmt.Errorf("重复的 repo %q", r.Repo)
		}
		seen[r.Repo] = true
	}
	return nil
}

// Slug 返回仓短名（"hoshinojian/nms" → "nms"）。
func (r *Repo) Slug() string {
	i := strings.IndexByte(r.Repo, '/')
	return r.Repo[i+1:]
}

// RepoTag 返回该仓 droplet 的 DO tag（编排器按 tag 对账归属）。
func (r *Repo) RepoTag() string { return "repo:" + r.Slug() }

// ServiceRepoPart 返回 systemd unit 中的仓名段（连字符，runbook 坑：写成点号会 Unit not found）。
func (r *Repo) ServiceRepoPart() string { return strings.ReplaceAll(r.Repo, "/", "-") }

// URL 返回仓的 HTTPS URL（runner config.sh --url）。
func (r *Repo) URL() string { return "https://github.com/" + r.Repo }
