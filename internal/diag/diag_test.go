package diag

import (
	"context"
	"strings"
	"testing"

	"github.com/hoshinojian/cirunner/internal/sys"
)

// fakeDiag：healthy 输出全链；指定的 section 前缀返回空+err 模拟读取失败。
type fakeDiag struct {
	failPrefix string
}

func (f *fakeDiag) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	switch {
	case strings.HasPrefix(key, "cloud-init status"):
		return []byte("status: done"), nil
	case strings.HasPrefix(key, "cat /var/lib/cirunner/stage"):
		return []byte("runner-started"), nil
	case strings.HasPrefix(key, "systemctl list-units actions.runner.*"):
		return []byte("actions.runner.demo-repo.mc-1.service loaded active running Runner\n"), nil
	case strings.HasPrefix(key, "systemctl show actions.runner"):
		return []byte("active"), nil
	case strings.HasPrefix(key, "systemctl show docker"):
		return []byte("active"), nil
	case strings.HasPrefix(key, "pgrep -f Runner.Listener"):
		return []byte("42\n"), nil
	case strings.HasPrefix(key, "df -P /"):
		return []byte("F 100 40 55 45% /\n"), nil
	case strings.HasPrefix(key, "free -m"), strings.HasPrefix(key, "cut -d"):
		return []byte("0 "), nil
	case strings.HasPrefix(key, "cut -d . -f1 /proc/uptime"):
		return []byte("900 "), nil
	case strings.HasPrefix(key, "tail -n "):
		if strings.HasPrefix(key, f.failPrefix) || strings.Contains(key, f.failPrefix) {
			return nil, context.DeadlineExceeded
		}
		return []byte("some log line\n"), nil
	}
	return []byte{}, nil
}

// §8.1：diag 单段失败不影响其他段（该段写 {"error"}）。
func TestSectionFailureIsolated(t *testing.T) {
	h := &sys.Host{Cmd: &fakeDiag{failPrefix: "cloud-init-output.log"}}
	r := Collect(context.Background(), h, 10)
	if !r.Health.OK {
		t.Fatalf("health 部分应正常: %+v", r.Health)
	}
	s, ok := r.Sections["cloud-init-output.log"]
	if !ok || s.Err == "" {
		t.Fatalf("失败段应有 error: %+v", r.Sections["cloud-init-output.log"])
	}
	if r.Sections["df-h"].Text == "" && r.Sections["df-h"].Err == "" {
		// df-h 走 Cmd 直跑不在 failPrefix 内
	}
	if _, ok := r.Sections["free-m"]; !ok {
		t.Fatal("其他段应照常采集")
	}
}

// 各段键齐全。
func TestAllSectionsPresent(t *testing.T) {
	r := Collect(context.Background(), &sys.Host{Cmd: &fakeDiag{}}, 10)
	want := []string{
		"cloud-init-output.log", "runner-journal", "runner-diag-log", "runner-worker-log",
		"docker-journal", "docker-ps", "df-h", "free-m", "dmesg-tail",
		"apt-20auto-upgrades", "apt-history",
	}
	for _, k := range want {
		if _, ok := r.Sections[k]; !ok {
			t.Errorf("缺段 %s", k)
		}
	}
}
