package act

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/cirunner/internal/sys"
)

type fakeAct struct {
	calls   []string
	units   []string
	states  map[string]string // unit → ActiveState
	pgrepOn string            // 判定运行的进程模式（"" = 都不在）
}

func (f *fakeAct) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	switch {
	case strings.HasPrefix(key, "systemctl list-units actions.runner.*"):
		out := ""
		for _, u := range f.units {
			out += u + " loaded active running Runner\n"
		}
		return []byte(out), nil
	case strings.HasPrefix(key, "systemctl show "):
		unit := strings.Fields(strings.TrimPrefix(key, "systemctl show "))[0]
		return []byte(f.states[unit]), nil
	case strings.HasPrefix(key, "pgrep -f "):
		pat := strings.TrimPrefix(key, "pgrep -f ")
		if pat == f.pgrepOn {
			return []byte("111\n"), nil
		}
		return []byte{}, nil
	}
	return []byte{}, nil
}

func hostWith(f *fakeAct) *sys.Host { return &sys.Host{Cmd: f} }

// restart-runner：重启全部实例并等到 Listener。
func TestRestartRunnerWaitsListener(t *testing.T) {
	f := &fakeAct{units: []string{"actions.runner.demo-repo.mc-1.service"}, pgrepOn: "Runner.Listener"}
	r := RestartRunner(context.Background(), hostWith(f))
	if !r.OK {
		t.Fatalf("应成功: %+v", r)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "systemctl restart actions.runner.demo-repo.mc-1.service") {
		t.Fatalf("应 restart 全部 unit: %v", f.calls)
	}
}

// restart 后 30s 内 Listener 未出现 → 失败（P139 建议 diag）。
func TestRestartRunnerListenerTimeout(t *testing.T) {
	f := &fakeAct{units: []string{"actions.runner.demo-repo.mc-1.service"}} // pgrepOn 空：Listener 不在
	orig := listenerWait
	listenerWait = 50 * time.Millisecond
	defer func() { listenerWait = orig }()
	r := RestartRunner(context.Background(), hostWith(f))
	if r.OK {
		t.Fatal("Listener 30s 未出现应失败")
	}
	// 缩短等待避免测试跑 30s：直接验证失败文案
	if !strings.Contains(r.Detail, "Listener") {
		t.Fatalf("失败详情应指向 Listener: %+v", r)
	}
}

// clean-work：服务在 active 时拒绝；停止后允许。
func TestCleanWorkGuardsActiveService(t *testing.T) {
	unit := "actions.runner.demo-repo.mc-1.service"
	f := &fakeAct{units: []string{unit}, states: map[string]string{unit: "active"}}
	if r := CleanWork(context.Background(), hostWith(f)); r.OK {
		t.Fatal("active 服务应拒绝 clean-work")
	}
	f2 := &fakeAct{units: []string{unit}, states: map[string]string{unit: "inactive"}}
	if r := CleanWork(context.Background(), hostWith(f2)); !r.OK {
		t.Fatalf("停止后应允许: %+v", r)
	}
	if !strings.Contains(strings.Join(f2.calls, "\n"), "rm -rf /opt/actions-runner*/_work/*") {
		t.Fatalf("应清理 _work: %v", f2.calls)
	}
}

// stop-runner：无 unit 时失败。
func TestStopRunnerNoUnits(t *testing.T) {
	f := &fakeAct{}
	if r := StopRunner(context.Background(), hostWith(f)); r.OK {
		t.Fatal("无 unit 应失败")
	}
}
