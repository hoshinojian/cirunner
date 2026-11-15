// Package engine 把三层粘起来：GitHub/DO 观察 → decide 决策 → vpsctl/gh 执行。
// 依赖注入（GH/VPS 接口），守护循环与手动命令共用。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hoshinojian/cirunner/internal/cloudinit"
	"github.com/hoshinojian/cirunner/internal/config"
	"github.com/hoshinojian/cirunner/internal/decide"
	"github.com/hoshinojian/cirunner/internal/githubapi"
	"github.com/hoshinojian/cirunner/internal/vpsctlapi"
)

// CirunnerTag 是编排器所辖 droplet 的公共 DO tag。
const CirunnerTag = "cirunner"

// GH 是 GitHub 侧能力（由 githubapi.Client 实现，测试可替换）。
type GH interface {
	QueuedRuns(ctx context.Context, repo string) (githubapi.QueuedInfo, error)
	Runners(ctx context.Context, repo string) ([]githubapi.Runner, error)
	MintToken(ctx context.Context, repo string) (string, error)
	RemoveRunner(ctx context.Context, repo string, id int64) error
}

// VPS 是 DO 侧能力（由 vpsctlapi.Client 实现，测试可替换）。
type VPS interface {
	Create(ctx context.Context, o vpsctlapi.CreateOpts) (vpsctlapi.Droplet, error)
	ListByTag(ctx context.Context, tag string) ([]vpsctlapi.Droplet, error)
	Delete(ctx context.Context, account string, id int64) error
}

// Engine 是编排器。
type Engine struct {
	Cfg        *config.Config
	GH         GH
	VPS        VPS
	PublicKeys []string
	Log        *log.Logger
	States     map[string]decide.State
	statePath  string
}

// New 构造编排器。
func New(cfg *config.Config, gh GH, vps VPS, publicKeys []string, lg *log.Logger) *Engine {
	if lg == nil {
		lg = log.Default()
	}
	return &Engine{
		Cfg: cfg, GH: gh, VPS: vps, PublicKeys: publicKeys, Log: lg,
		States: map[string]decide.State{},
	}
}

// LoadStates 从磁盘恢复决策记忆（空文件/缺文件视为全新）。
func (e *Engine) LoadStates(path string) error {
	e.statePath = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &e.States)
}

// SaveStates 持久化决策记忆（原子写）。
func (e *Engine) SaveStates() error {
	if e.statePath == "" {
		return nil
	}
	raw, err := json.MarshalIndent(e.States, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.statePath), 0o700); err != nil {
		return err
	}
	tmp := e.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.statePath)
}

// Observe 采集一个租户仓的世界快照（全部 GitHub 侧信号 + 本仓 droplet）。
func (e *Engine) Observe(ctx context.Context, repo config.Repo) (decide.Obs, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	q, err := e.GH.QueuedRuns(ctx, repo.Repo)
	if err != nil {
		return decide.Obs{}, fmt.Errorf("%s 观察排队: %w", repo.Repo, err)
	}
	runners, err := e.GH.Runners(ctx, repo.Repo)
	if err != nil {
		return decide.Obs{}, fmt.Errorf("%s 观察 runner: %w", repo.Repo, err)
	}
	droplets, err := e.VPS.ListByTag(ctx, repo.RepoTag())
	if err != nil {
		return decide.Obs{}, fmt.Errorf("%s 观察 droplet: %w", repo.Repo, err)
	}

	obs := decide.Obs{Now: q.ObservedAt, QueuedRuns: q.Count, OldestQueuedAge: q.OldestAge}
	for _, d := range droplets {
		obs.Droplets = append(obs.Droplets, decide.Droplet{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt})
	}
	for _, r := range runners {
		obs.Runners = append(obs.Runners, decide.Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy})
	}
	return obs, nil
}

// decideCfg 把仓配置映射为决策参数。
func decideCfg(r config.Repo, globalIdleMin int) decide.Cfg {
	idle := time.Duration(globalIdleMin) * time.Minute
	if idle == 0 {
		idle = 20 * time.Minute
	}
	return decide.Cfg{NamePrefix: r.NamePrefix, MaxRunners: r.MaxRunners, IdleTimeout: idle}
}

// Reconcile 对一个租户仓走一轮 观察→决策→执行。budget 为全局剩余可建配额
// （每建一台扣 1；扣到 0 后 create 动作被跳过并记日志）。dryRun 只打印不执行。
func (e *Engine) Reconcile(ctx context.Context, repo config.Repo, dryRun bool, budget *int) ([]decide.Action, error) {
	obs, err := e.Observe(ctx, repo)
	if err != nil {
		return nil, err
	}
	acts, st := decide.Decide(decideCfg(repo, e.Cfg.IdleTimeoutMin), e.States[repo.Repo], obs)
	e.States[repo.Repo] = st
	if dryRun {
		return acts, nil
	}
	for _, a := range acts {
		if err := e.execute(ctx, repo, obs, a, budget); err != nil {
			return acts, fmt.Errorf("执行 %s: %w", a.Kind, err)
		}
	}
	return acts, nil
}

func (e *Engine) execute(ctx context.Context, repo config.Repo, obs decide.Obs, a decide.Action, budget *int) error {
	switch a.Kind {
	case decide.ActCreate:
		if budget != nil && *budget <= 0 {
			e.Log.Printf("[%s] 跳过建机：全局 droplet 上限 %d 已满", repo.Repo, e.Cfg.MaxDroplets)
			return nil
		}
		d, err := e.CreateFor(ctx, repo, obs)
		if err != nil {
			return err
		}
		if budget != nil {
			*budget--
		}
		e.Log.Printf("[%s] 已建机 %s (id=%d, ip=%s)", repo.Repo, d.Name, d.ID, d.IPv4Public)
	case decide.ActDeleteDroplet:
		if err := e.VPS.Delete(ctx, repo.Account, a.DropletID); err != nil {
			return err
		}
		e.Log.Printf("[%s] 已删机 id=%d（%s）", repo.Repo, a.DropletID, a.Reason)
	case decide.ActRemoveRegistration:
		for _, r := range obs.Runners {
			if r.Name == a.RunnerName {
				if err := e.GH.RemoveRunner(ctx, repo.Repo, r.ID); err != nil {
					return err
				}
				e.Log.Printf("[%s] 已清理残留注册 %s (id=%d)", repo.Repo, r.Name, r.ID)
				break
			}
		}
	case decide.ActAlert:
		e.Log.Printf("[%s] 告警: %s", repo.Repo, a.Reason)
	}
	return nil
}

// CreateFor 走完整建机流程：铸 token → 渲染 cloud-init → vpsctl create。
// runner 名先于建机确定（droplet 名 = runner 名，decide 按名对账）。
func (e *Engine) CreateFor(ctx context.Context, repo config.Repo, obs decide.Obs) (vpsctlapi.Droplet, error) {
	if len(e.PublicKeys) == 0 {
		return vpsctlapi.Droplet{}, fmt.Errorf("未配置 ssh 公钥（ssh_public_key_file），无法注入 cloud-init")
	}
	name, err := NextName(repo, obs.Droplets)
	if err != nil {
		return vpsctlapi.Droplet{}, err
	}

	tokCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	token, err := e.GH.MintToken(tokCtx, repo.Repo)
	cancel()
	if err != nil {
		return vpsctlapi.Droplet{}, err
	}

	ci, err := cloudinit.Render(cloudinit.Params{
		RepoURL:     repo.URL(),
		ServiceRepo: repo.ServiceRepoPart(),
		RunnerName:  name,
		Labels:      repo.Labels,
		Token:       token,
		PublicKeys:  e.PublicKeys,
	})
	if err != nil {
		return vpsctlapi.Droplet{}, fmt.Errorf("渲染 cloud-init: %w", err)
	}

	dir := filepath.Join(e.stateDir(), "cloudinit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vpsctlapi.Droplet{}, err
	}
	// 文件含 1h 时效 token：建机成功即删；失败留存供排查（目录 0700）。
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, ci, 0o600); err != nil {
		return vpsctlapi.Droplet{}, err
	}

	tags := append([]string{CirunnerTag, repo.RepoTag()}, repo.Tags...)
	idx := nameIndex(repo, name)
	createCtx, cancel2 := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel2()
	d, err := e.VPS.Create(createCtx, vpsctlapi.CreateOpts{
		Account:      repo.Account,
		Region:       repo.Region,
		Size:         repo.Size,
		Image:        repo.Image,
		NamePrefix:   repo.NamePrefix,
		StartIndex:   idx,
		Tags:         tags,
		UserDataFile: path,
		Wait:         5 * time.Minute,
	})
	if err != nil {
		return d, err
	}
	_ = os.Remove(path)
	return d, nil
}

func (e *Engine) stateDir() string {
	if e.Cfg.StateDir != "" {
		return e.Cfg.StateDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "cirunner")
}

var nameSuffixRe = regexp.MustCompile(`^(\d+)$`)

// NextName 计算下一台 droplet 名：在现存同名空间里取最大序号 +1
// （与 vpsctl {prefix}-{account}-{region}-%02d 命名一致，序号显式传 -start-index）。
func NextName(r config.Repo, existing []decide.Droplet) (string, error) {
	pre := fmt.Sprintf("%s-%s-%s-", r.NamePrefix, r.Account, r.Region)
	max := 0
	for _, d := range existing {
		suffix, ok := strings.CutPrefix(d.Name, pre)
		if !ok {
			continue
		}
		m := nameSuffixRe.FindStringSubmatch(suffix)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s%02d", pre, max+1), nil
}

// nameIndex 从完整名提取序号（作 -start-index）。
func nameIndex(r config.Repo, name string) int {
	pre := fmt.Sprintf("%s-%s-%s-", r.NamePrefix, r.Account, r.Region)
	suffix, _ := strings.CutPrefix(name, pre)
	n, _ := strconv.Atoi(suffix)
	return n
}

// CountAllDroplets 统计编排器名下全部 droplet（全局配额用）。
func (e *Engine) CountAllDroplets(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ds, err := e.VPS.ListByTag(ctx, CirunnerTag)
	return len(ds), err
}

// Teardown 强制回收一个租户仓的全部 droplet 与对应注册（down 命令）。
func (e *Engine) Teardown(ctx context.Context, repo config.Repo) error {
	obs, err := e.Observe(ctx, repo)
	if err != nil {
		return err
	}
	for _, d := range obs.Droplets {
		if err := e.VPS.Delete(ctx, repo.Account, d.ID); err != nil {
			return fmt.Errorf("删机 %s: %w", d.Name, err)
		}
		e.Log.Printf("[%s] down: 已删机 %s (id=%d)", repo.Repo, d.Name, d.ID)
	}
	// 清理本命名空间的残留注册（在线的也清——机器已经没了）。
	for _, r := range obs.Runners {
		if MatchesNaming(repo, r.Name) {
			if err := e.GH.RemoveRunner(ctx, repo.Repo, r.ID); err != nil {
				return fmt.Errorf("清注册 %s: %w", r.Name, err)
			}
			e.Log.Printf("[%s] down: 已清注册 %s (id=%d)", repo.Repo, r.Name, r.ID)
		}
	}
	delete(e.States, repo.Repo)
	return nil
}

// MatchesNaming 判断 runner/droplet 名是否属于该仓的命名空间
// （{prefix}-{account}-{region}-NN；外部 runner 如 pilot 期旧手工机不匹配）。
func MatchesNaming(r config.Repo, name string) bool {
	pre := fmt.Sprintf("%s-%s-%s-", r.NamePrefix, r.Account, r.Region)
	suffix, ok := strings.CutPrefix(name, pre)
	if !ok {
		return false
	}
	return nameSuffixRe.MatchString(suffix)
}
