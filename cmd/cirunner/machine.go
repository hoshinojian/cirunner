// 机器侧子命令（REDESIGN §5.4）：health / diag / act。
// 这些命令在 runner droplet 上以 root 运行，不需要编排器配置——在 main
// 的配置加载之前拦截分发。退出码：0 成功；1 检查不通过/动作失败；2 用法错误。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hoshinojian/cirunner/internal/act"
	"github.com/hoshinojian/cirunner/internal/diag"
	"github.com/hoshinojian/cirunner/internal/health"
	"github.com/hoshinojian/cirunner/internal/sys"
)

// machineCommands 是机器侧子命令集合。
var machineCommands = map[string]bool{"health": true, "diag": true, "act": true}

// runMachineCommand 处理 health/diag/act；返回 (handled, exitCode)。
func runMachineCommand(sub string, args []string) (bool, int) {
	if !machineCommands[sub] {
		return false, 0
	}
	// §5.4 的用法是 flag 在位置参数之后（act <name> --json）：Go flag 遇首个
	// 位置参数即停，后面的 flag 会落进 Args()——先手工分离再 Parse。
	var flagArgs, positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "输出 JSON（编排器消费；默认人读中文）")
	lines := fs.Int("lines", 200, "diag：各文本段截取的末尾行数")
	if err := fs.Parse(flagArgs); err != nil {
		return true, 2
	}

	requireRoot := func() bool {
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "cirunner: 机器侧命令需要 root（经 ssh 以 root 执行）")
			return false
		}
		return true
	}

	switch sub {
	case "health":
		if !requireRoot() {
			return true, 1
		}
		host := sys.NewHost()
		ctx := context.Background()
		r := health.Collect(ctx, host)
		if *jsonOut {
			emitJSON(r)
		} else {
			printHealthHuman(r)
		}
		if !r.OK {
			return true, 1
		}
		return true, 0
	case "diag":
		if !requireRoot() {
			return true, 1
		}
		host := sys.NewHost()
		ctx := context.Background()
		r := diag.Collect(ctx, host, *lines)
		if *jsonOut {
			emitJSON(r)
		} else {
			printDiagHuman(r)
		}
		if !r.Health.OK {
			return true, 1
		}
		return true, 0
	case "act":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "用法: cirunner act <restart-runner|stop-runner|clean-work> --json")
			return true, 2
		}
		var r act.Result
		switch positional[0] {
		case "restart-runner", "stop-runner", "clean-work":
			if !requireRoot() {
				return true, 1
			}
			host := sys.NewHost()
			ctx := context.Background()
			switch positional[0] {
			case "restart-runner":
				r = act.RestartRunner(ctx, host)
			case "stop-runner":
				r = act.StopRunner(ctx, host)
			case "clean-work":
				r = act.CleanWork(ctx, host)
			}
		default:
			fmt.Fprintf(os.Stderr, "cirunner: 未知动作 %q（白名单：restart-runner/stop-runner/clean-work）\n", positional[0])
			return true, 2
		}
		if *jsonOut {
			emitJSON(r)
		} else {
			fmt.Printf("动作 %s：%s\n", r.Action, detail(r))
		}
		if !r.OK {
			return true, 1
		}
		return true, 0
	}
	return true, 2
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func detail(r act.Result) string {
	if r.OK {
		return "成功——" + r.Detail
	}
	return "失败——" + r.Detail
}

func printHealthHuman(r health.Report) {
	c := r.Checks
	status := "健康"
	if !r.OK {
		status = "异常"
	}
	fmt.Printf("总评：%s\n", status)
	fmt.Printf("cloud-init：%s（阶段 %s）\n", c.CloudInit.Status, orUnknown(c.CloudInit.Stage))
	if len(c.RunnerServices) == 0 {
		fmt.Println("runner 服务：无")
	}
	for _, s := range c.RunnerServices {
		fmt.Printf("runner 服务：%s（%s）\n", s.Unit, s.Active)
	}
	fmt.Printf("Listener 进程：%s\n", procHuman(c.ListenerProcess))
	fmt.Printf("Worker 进程：%s\n", procHuman(c.WorkerProcess))
	fmt.Printf("docker：%v\n", c.Docker.Active)
	fmt.Printf("磁盘可用：%.1f%%\n", c.DiskRootFreePct)
	fmt.Printf("可用内存：%d MB\n", c.MemAvailableMB)
	fmt.Printf("load1：%.2f\n", c.Load1)
	fmt.Printf("runner 版本：%s\n", orUnknown(c.RunnerVersion))
	fmt.Printf("开机时长：%s\n", uptimeHuman(c.UptimeSec))
}

func printDiagHuman(r diag.Report) {
	printHealthHuman(r.Health)
	fmt.Println("—— 诊断材料段 ——")
	for _, key := range []string{
		"cloud-init-output.log", "runner-journal", "runner-diag-log", "runner-worker-log",
		"docker-journal", "docker-ps", "df-h", "free-m", "dmesg-tail",
		"apt-20auto-upgrades", "apt-history",
	} {
		s, ok := r.Sections[key]
		if !ok {
			continue
		}
		fmt.Printf("===== %s =====\n", key)
		if s.Err != "" {
			fmt.Printf("  [读取失败] %s\n", s.Err)
			continue
		}
		fmt.Println(s.Text)
	}
}

func procHuman(p health.Process) string {
	if !p.Running {
		return "不在"
	}
	if p.PID > 0 {
		return fmt.Sprintf("运行中（pid %d）", p.PID)
	}
	return "运行中"
}

func orUnknown(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

func uptimeHuman(sec int) string {
	if sec <= 0 {
		return "未知"
	}
	h, m := sec/3600, (sec%3600)/60
	return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
}
