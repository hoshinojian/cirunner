package health

import (
	"context"
	"strings"
	"testing"

	"github.com/hoshinojian/cirunner/internal/sys"
)

// fakeCmd 按前缀匹配返回预置输出（fake 掉全部子进程；后 add 的先匹配）。
type fakeCmd struct{ rules []fakeRule }

type fakeRule struct {
	prefix string
	out    string
}

func (f *fakeCmd) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	for i := len(f.rules) - 1; i >= 0; i-- {
		if strings.HasPrefix(key, f.rules[i].prefix) {
			return []byte(f.rules[i].out), nil
		}
	}
	return []byte{}, nil
}

func (f *fakeCmd) add(prefix, out string) { f.rules = append(f.rules, fakeRule{prefix, out}) }

// set 覆盖已 add 的第一条同前缀规则。
func (f *fakeCmd) set(prefix, out string) {
	for i := range f.rules {
		if f.rules[i].prefix == prefix {
			f.rules[i].out = out
			return
		}
	}
	f.add(prefix, out)
}

func healthyHost() *sys.Host {
	fc := &fakeCmd{}
	fc.add("cloud-init status", "status: done")
	fc.add("cat /var/lib/cirunner/stage", "runner-started")
	fc.add("systemctl list-units actions.runner.*", "actions.runner.demo-repo.mc-1.service loaded active running Runner\n")
	fc.add("systemctl show actions.runner.demo-repo.mc-1.service", "active")
	fc.add("pgrep -f Runner.Listener", "4242\n")
	fc.add("systemctl show docker", "active")
	fc.add("df -P /", "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/vda1 100 40 55 42% /")
	fc.add("free -m", "Mem: 8000 2000 5900 100 100 5800")
	fc.add("cut -d   -f1 /proc/loadavg", "0.30 ")
	fc.add("cut -d . -f1 /proc/uptime", "900 ")
	fc.add("sh -c for d in", "2.329.0")
	return &sys.Host{Cmd: fc}
}

func TestHealthyMachineOK(t *testing.T) {
	r := Collect(context.Background(), healthyHost())
	if !r.OK {
		t.Fatalf("健康机器应 OK: %+v", r.Checks)
	}
	if r.Checks.CloudInit.Status != "done" || r.Checks.CloudInit.Stage != "runner-started" {
		t.Fatalf("cloud-init 判定不对: %+v", r.Checks.CloudInit)
	}
	if !r.Checks.ListenerProcess.Running || r.Checks.ListenerProcess.PID != 4242 {
		t.Fatalf("Listener 判定不对: %+v", r.Checks.ListenerProcess)
	}
	if r.Checks.DiskRootFreePct != 55 || r.Checks.MemAvailableMB != 5800 {
		t.Fatalf("资源判定不对: %+v", r.Checks)
	}
	if r.Checks.RunnerVersion != "2.329.0" || r.Checks.UptimeSec != 900 {
		t.Fatalf("版本/时长不对: %+v", r.Checks)
	}
}

// §5.4 ok=false 条件逐项：cloud-init error、服务非 active、Listener 不在、
// docker 非 active、磁盘 <10%。
func TestUnhealthyConditions(t *testing.T) {
	cases := map[string]func(*fakeCmd){
		"cloud-init error": func(f *fakeCmd) { f.set("cloud-init status", "status: error") },
		"服务非 active":       func(f *fakeCmd) { f.set("systemctl show actions.runner", "failed") },
		"Listener 不在":      func(f *fakeCmd) { f.set("pgrep -f Runner.Listener", "") },
		"docker 非 active":  func(f *fakeCmd) { f.set("systemctl show docker", "inactive") },
		"磁盘 <10%": func(f *fakeCmd) {
			f.set("df -P /", "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/vda1 100 95 4 96% /")
		},
	}
	for name, mutate := range cases {
		h := healthyHost()
		f := h.Cmd.(*fakeCmd)
		mutate(f)
		if r := Collect(context.Background(), h); r.OK {
			t.Errorf("%s 应 ok=false", name)
		}
	}
}

// Worker 不在不算异常（没跑 job 时正常）。
func TestWorkerAbsentStillOK(t *testing.T) {
	h := healthyHost()
	f := h.Cmd.(*fakeCmd)
	f.add("pgrep -f Runner.Worker", "")
	r := Collect(context.Background(), h)
	if !r.OK {
		t.Fatal("Worker 不在应仍 OK")
	}
	if r.Checks.WorkerProcess.Running {
		t.Fatal("Worker 应判不在")
	}
}
