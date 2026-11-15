// cirunner — 按需 GitHub Actions 自建 runner 池编排器。
//
// 用法:
//
//	cirunner -config <path> status              观察各租户仓的队列/runner/droplet
//	cirunner -config <path> plan                观察并输出本轮会执行的决策（不执行）
//	cirunner -config <path> up <repo>           手动强制建一台（repo 支持全名或短名）
//	cirunner -config <path> down <repo> [-force] 手动回收该仓全部 droplet 与注册
//	cirunner -config <path> daemon              守护循环（队列起停，建议 systemd 托管）
//
// 设计与踩坑来源见 nms 仓 docs/runbooks/gitrunner.md 与 pitfalls P139。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hoshinojian/cirunner/internal/config"
	"github.com/hoshinojian/cirunner/internal/engine"
	"github.com/hoshinojian/cirunner/internal/githubapi"
	"github.com/hoshinojian/cirunner/internal/vpsctlapi"
)

func main() {
	// 预扫描：-config <path> / -config=<path> 允许出现在子命令前或后
	// （`cirunner -config X daemon` 与 `cirunner daemon -config X` 等价）。
	var cfgPath string
	var rest []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config":
			if i+1 >= len(args) {
				fatal(fmt.Errorf("-config 需要参数"))
			}
			cfgPath = args[i+1]
			i++
		case strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(strings.TrimPrefix(a, "--config="), "-config=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		usage()
		os.Exit(2)
	}
	sub := rest[0]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	var force bool
	if sub == "down" {
		fs.BoolVar(&force, "force", false, "有排队 run 也强制回收")
	}
	if err := fs.Parse(rest[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() > 0 && sub != "up" && sub != "down" {
		fatal(fmt.Errorf("子命令 %s 不接受位置参数 %v", sub, fs.Args()))
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal(err)
	}
	pkLines, err := readPublicKeys(cfg.SSHPublicKeyFile)
	if err != nil {
		fatal(err)
	}

	gh := githubapi.NewClient(cfg.GhBin)
	vps := vpsctlapi.NewClient(cfg.VPSctlBin, cfg.AccountsFile)
	eng := engine.New(cfg, gh, vps, pkLines, nil)
	statePath := filepath.Join(stateDirOf(cfg), "states.json")
	if err := eng.LoadStates(statePath); err != nil {
		fatal(fmt.Errorf("恢复状态: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "status":
		err = cmdStatus(ctx, cfg, eng)
	case "plan":
		err = cmdPlan(ctx, cfg, eng)
	case "up":
		if fs.NArg() != 1 {
			fatal(fmt.Errorf("用法: cirunner up <owner/name|短名>"))
		}
		err = cmdUp(ctx, cfg, eng, fs.Arg(0))
	case "down":
		if fs.NArg() != 1 {
			fatal(fmt.Errorf("用法: cirunner down <owner/name|短名> [-force]"))
		}
		err = cmdDown(ctx, cfg, eng, fs.Arg(0), force)
	case "daemon":
		err = cmdDaemon(ctx, cfg, eng, statePath)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cirunner — 按需 GitHub Actions 自建 runner 池

用法:
  cirunner [-config path] status                 观察队列/runner/droplet
  cirunner [-config path] plan                   输出本轮决策（不执行）
  cirunner [-config path] up <repo>              手动建一台
  cirunner [-config path] down <repo> [-force]   手动回收
  cirunner [-config path] daemon                 守护循环
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "cirunner:", err)
	os.Exit(1)
}

func stateDirOf(cfg *config.Config) string {
	if cfg.StateDir != "" {
		return cfg.StateDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "cirunner")
}

func readPublicKeys(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("配置缺 ssh_public_key_file（cloud-init 需自带公钥）")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s 中无有效公钥", path)
	}
	return lines, nil
}

func findRepo(cfg *config.Config, sel string) (config.Repo, error) {
	for _, r := range cfg.Repos {
		if r.Repo == sel || r.Slug() == sel {
			return r, nil
		}
	}
	return config.Repo{}, fmt.Errorf("配置中无仓 %q（repos: %v）", sel, repoNames(cfg))
}

func repoNames(cfg *config.Config) []string {
	var out []string
	for _, r := range cfg.Repos {
		out = append(out, r.Repo)
	}
	return out
}

func cmdStatus(ctx context.Context, cfg *config.Config, eng *engine.Engine) error {
	for _, r := range cfg.Repos {
		obs, err := eng.Observe(ctx, r)
		if err != nil {
			return err
		}
		fmt.Printf("%s  排队 run: %d", r.Repo, obs.QueuedRuns)
		if obs.QueuedRuns > 0 {
			fmt.Printf("（最老 %s）", obs.OldestQueuedAge.Round(time.Second))
		}
		fmt.Println()
		for _, d := range obs.Droplets {
			fmt.Printf("  droplet %-32s id=%d\n", d.Name, d.ID)
		}
		if len(obs.Droplets) == 0 {
			fmt.Println("  droplet（无）")
		}
		for _, rn := range obs.Runners {
			busy := "空闲"
			if rn.Busy {
				busy = "忙碌"
			}
			fmt.Printf("  runner %-32s %s %s\n", rn.Name, rn.Status, busy)
		}
		if len(obs.Runners) == 0 {
			fmt.Println("  runner（无注册）")
		}
	}
	return nil
}

func cmdPlan(ctx context.Context, cfg *config.Config, eng *engine.Engine) error {
	budget, err := budgetOf(ctx, cfg, eng)
	if err != nil {
		return err
	}
	fmt.Printf("全局配额剩余: %d/%d\n", budget, cfg.MaxDroplets)
	for _, r := range cfg.Repos {
		acts, err := eng.Reconcile(ctx, r, true, &budget)
		if err != nil {
			return err
		}
		if len(acts) == 0 {
			fmt.Printf("%s  本轮无动作\n", r.Repo)
			continue
		}
		for _, a := range acts {
			fmt.Printf("%s  [计划] %s %d %s — %s\n", r.Repo, a.Kind, a.DropletID, a.RunnerName, a.Reason)
		}
	}
	return nil
}

func budgetOf(ctx context.Context, cfg *config.Config, eng *engine.Engine) (int, error) {
	n, err := eng.CountAllDroplets(ctx)
	if err != nil {
		return 0, err
	}
	return cfg.MaxDroplets - n, nil
}

func cmdUp(ctx context.Context, cfg *config.Config, eng *engine.Engine, sel string) error {
	r, err := findRepo(cfg, sel)
	if err != nil {
		return err
	}
	obs, err := eng.Observe(ctx, r)
	if err != nil {
		return err
	}
	d, err := eng.CreateFor(ctx, r, obs)
	if err != nil {
		return err
	}
	fmt.Printf("已建机 %s (id=%d, ip=%s)——runner 约数分钟内上线（cloud-init 装底座）\n", d.Name, d.ID, d.IPv4Public)
	return nil
}

func cmdDown(ctx context.Context, cfg *config.Config, eng *engine.Engine, sel string, force bool) error {
	r, err := findRepo(cfg, sel)
	if err != nil {
		return err
	}
	obs, err := eng.Observe(ctx, r)
	if err != nil {
		return err
	}
	if obs.QueuedRuns > 0 && !force {
		return fmt.Errorf("尚有 %d 个排队 run，拒绝回收；确要回收加 -force", obs.QueuedRuns)
	}
	if err := eng.Teardown(ctx, r); err != nil {
		return err
	}
	if err := eng.SaveStates(); err != nil {
		return err
	}
	fmt.Printf("%s 已回收（droplet 与注册清理完毕）\n", r.Repo)
	return nil
}

func cmdDaemon(ctx context.Context, cfg *config.Config, eng *engine.Engine, statePath string) error {
	interval := time.Duration(cfg.PollIntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	fmt.Printf("cirunner daemon 启动：采样间隔 %s，租户 %v\n", interval, repoNames(cfg))
	for {
		if err := runCycle(ctx, cfg, eng, statePath); err != nil {
			fmt.Fprintln(os.Stderr, "cycle:", err)
		}
		select {
		case <-ctx.Done():
			fmt.Println("daemon 退出")
			return nil
		case <-time.After(interval):
		}
	}
}

func runCycle(ctx context.Context, cfg *config.Config, eng *engine.Engine, statePath string) error {
	budget, err := budgetOf(ctx, cfg, eng)
	if err != nil {
		return fmt.Errorf("统计 droplet: %w", err)
	}
	for _, r := range cfg.Repos {
		acts, err := eng.Reconcile(ctx, r, false, &budget)
		if err != nil {
			fmt.Fprintln(os.Stderr, r.Repo, "reconcile:", err)
			continue
		}
		for _, a := range acts {
			fmt.Printf("%s  %s — %s\n", r.Repo, a.Kind, a.Reason)
		}
	}
	return eng.SaveStates()
}
