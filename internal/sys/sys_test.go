package sys

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSys 按"命令前缀"喂输出（后 add 的先匹配；err 模拟非零退出码）。
type fakeSys struct {
	rules []fakeRule
}

type fakeRule struct {
	prefix string
	out    string
	err    error
}

func (f *fakeSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	for i := len(f.rules) - 1; i >= 0; i-- {
		if strings.HasPrefix(key, f.rules[i].prefix) {
			if f.rules[i].err != nil {
				return []byte(f.rules[i].out), f.rules[i].err
			}
			return []byte(f.rules[i].out), nil
		}
	}
	return []byte{}, nil
}

func (f *fakeSys) add(prefix, out string) {
	f.rules = append(f.rules, fakeRule{prefix: prefix, out: out})
}
func (f *fakeSys) addErr(prefix, out string, err error) {
	f.rules = append(f.rules, fakeRule{prefix, out, err})
}

// df -P 真实输出样本：Available 是第 4 列不是第 5 列（Capacity）——
// 此前取错列把已用百分比当成了可用百分比。
func TestDiskRootFreePct(t *testing.T) {
	f := &fakeSys{}
	// total=100, used=40, avail=55 → 55%
	f.add("df -P /", "Filesystem     1024-blocks      Used Available Capacity Mounted on\n/dev/vda1            102400      40960     55296      43% /\n")
	h := &Host{Cmd: f}
	got, err := h.DiskRootFreePct(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got < 53.9 || got > 54.1 { // 55296/102400 = 54.0
		t.Fatalf("应为 54.0（avail/total），got %.2f", got)
	}
	// 异常输出报错而非误判
	f2 := &fakeSys{}
	f2.add("df -P /", "garbage\n")
	if _, err := (&Host{Cmd: f2}).DiskRootFreePct(context.Background()); err == nil {
		t.Fatal("异常 df 输出应报错")
	}
}

// pgrep 空输出 = 不在（此前空+nil 会误判运行中）；多行 = 计数。
func TestPgrep(t *testing.T) {
	f := &fakeSys{}
	f.add("pgrep -f Runner.Listener", "111\n222\n") // rpd=2 双实例
	h := &Host{Cmd: f}
	if ok, pid, _ := h.PgrepRunning(context.Background(), "Runner.Listener"); !ok || pid != 111 {
		t.Fatalf("应运行 pid=111: %v %d", ok, pid)
	}
	if n := h.PgrepCount(context.Background(), "Runner.Listener"); n != 2 {
		t.Fatalf("应计 2 个，got %d", n)
	}
	f2 := &fakeSys{}
	f2.add("pgrep -f Runner.Worker", "") // 无匹配：真实 pgrep 退出码非零，fake 空输出
	h2 := &Host{Cmd: f2}
	if ok, _, _ := h2.PgrepRunning(context.Background(), "Runner.Worker"); ok {
		t.Fatal("空输出应判不在")
	}
	if n := h2.PgrepCount(context.Background(), "Runner.Worker"); n != 0 {
		t.Fatal("空输出计数应为 0")
	}
}

// RunnerVersion：config.sh --version 输出（含换行）。
func TestRunnerVersion(t *testing.T) {
	f := &fakeSys{}
	f.add("sh -c for d in", "2.338.2\n")
	h := &Host{Cmd: f}
	if v := h.RunnerVersion(context.Background()); v != "2.338.2" {
		t.Fatalf("got %q", v)
	}
	f2 := &fakeSys{} // 无 config.sh：输出空
	if v := (&Host{Cmd: f2}).RunnerVersion(context.Background()); v != "" {
		t.Fatalf("应空，got %q", v)
	}
}

// cloud-init status：done/running/正交所 error（非零退出+status: error）。
func TestCloudInitStatus(t *testing.T) {
	mk := func(out string, err error) *Host {
		f := &fakeSys{}
		if err != nil {
			f.addErr("cloud-init status", out, err)
		} else {
			f.add("cloud-init status", out)
		}
		return &Host{Cmd: f}
	}
	if s := mk("status: done\n", nil).CloudInitStatus(context.Background()); s != "done" {
		t.Fatalf("done: got %q", s)
	}
	if s := mk("status: running\n", nil).CloudInitStatus(context.Background()); s != "running" {
		t.Fatalf("running: got %q", s)
	}
	if s := mk("status: error\n", errors.New("exit 1")).CloudInitStatus(context.Background()); s != "error" {
		t.Fatalf("error: got %q", s)
	}
	if s := mk("", errors.New("exit 2")).CloudInitStatus(context.Background()); s != "unknown" {
		t.Fatalf("unknown: got %q", s)
	}
}

// RunnerUnits 解析 list-units 输出（含非 runner 行过滤）。
func TestRunnerUnits(t *testing.T) {
	f := &fakeSys{}
	f.add("systemctl list-units actions.runner.*",
		"actions.runner.demo-repo.mc-1.service loaded active running Runner\n"+
			"actions.runner.demo-repo.mc-1-a.service loaded active running Runner\n"+
			"UNIT LOAD ACTIVE SUB DESCRIPTION\n")
	units, err := (&Host{Cmd: f}).RunnerUnits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0] != "actions.runner.demo-repo.mc-1.service" {
		t.Fatalf("解析不对: %v", units)
	}
}
