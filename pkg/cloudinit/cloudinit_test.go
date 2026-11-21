package cloudinit

import (
	"os"
	"strings"
	"testing"
)

func validParams() Params {
	return Params{
		RepoURL:      "https://github.com/hoshinojian/nms",
		ServiceRepo:  "hoshinojian-nms",
		RunnerName:   "ci-nms-team2-ci-sgp1-01",
		Labels:       []string{"ci"},
		Token:        "AGB2ZQKHL4TGHTRSEXAMPLE",
		PublicKeys:   []string{"ssh-ed25519 AAAA orchestrator", "ssh-ed25519 BBBB maintainer"},
		AgentVersion: "v0.1.0",
		AgentSHA256:  strings.Repeat("a", 64),
	}
}

func renderOK(t *testing.T, p Params) string {
	t.Helper()
	out, err := Render(p)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	return string(out)
}

// 坑 1：unattended-upgrades 必须显式关闭。
func TestPitfall1UnattendedUpgradesOff(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, `APT::Periodic::Unattended-Upgrade "0"`) {
		t.Fatal("坑 1：应包含 Unattended-Upgrade \"0\"")
	}
}

// 坑 2：runner 一律 latest + 版本号格式守卫。
func TestPitfall2RunnerLatestWithGuard(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "releases/latest") {
		t.Fatal("坑 2：应从 latest release 下载")
	}
	if !strings.Contains(s, `''|*[!0-9.]*)`) {
		t.Fatal("坑 2：应带版本号格式守卫（非数字版本 exit 1）")
	}
}

// 坑 3：daemon-reload 必须在 start 之前（同一实例块内顺序）。
func TestPitfall3DaemonReloadBeforeStart(t *testing.T) {
	s := renderOK(t, validParams())
	for _, inst := range []string{"ci-nms-team2-ci-sgp1-01"} {
		dr := strings.Index(s, "systemctl daemon-reload\n    systemctl start actions.runner.hoshinojian-nms."+inst)
		if dr < 0 {
			t.Fatalf("坑 3：%s 的 daemon-reload 应紧邻 start 之前", inst)
		}
	}
}

// 坑 4：unit 仓名用连字符（模板注入 ServiceRepo，不含点号/斜杠）。
func TestPitfall4ServiceRepoHyphenated(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "actions.runner.hoshinojian-nms.ci-nms-team2-ci-sgp1-01.service") {
		t.Fatal("坑 4：unit 名应为连字符仓名段")
	}
}

// 坑 5：公钥走 ssh_authorized_keys（模板自带，不依赖 vpsctl 注入）。
func TestPitfall5AuthorizedKeysInTemplate(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "ssh_authorized_keys:") ||
		!strings.Contains(s, "ssh-ed25519 AAAA orchestrator") ||
		!strings.Contains(s, "ssh-ed25519 BBBB maintainer") {
		t.Fatal("坑 5：编排器与维护者公钥都应在 ssh_authorized_keys")
	}
}

// 坑 6：纯 ASCII；文件内容走 write_files（runcmd 里不 printf 文件）。
func TestPitfall6ASCIIAndWriteFiles(t *testing.T) {
	s := renderOK(t, validParams())
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			t.Fatalf("坑 6：offset %d 含非 ASCII 字节", i)
		}
	}
	if !strings.Contains(s, "write_files:") || !strings.Contains(s, "/etc/apt/apt.conf.d/20auto-upgrades") {
		t.Fatal("坑 6：apt 配置应走 write_files block scalar")
	}
	if strings.Contains(s, "printf 'APT::") {
		t.Fatal("坑 6：runcmd 内不应 printf 文件内容（P70）")
	}
}

// 坑 7：root 密码过期解除 + 禁密码登录。
func TestPitfall7RootChageAndNoPassword(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, `chage -d "$(date +%F)" root`) {
		t.Fatal("坑 7：应解除 root 密码过期")
	}
	if !strings.Contains(s, "ssh_pwauth: false") {
		t.Fatal("坑 7：应禁密码登录")
	}
}

// 坑 8：禁 ssh.socket 直跑 ssh.service；sshd 双听 22/443。
func TestPitfall8SocketTrapAndDualPort(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "systemctl disable --now ssh.socket") {
		t.Fatal("坑 8：应禁 ssh.socket")
	}
	if !strings.Contains(s, "systemctl enable ssh.service") {
		t.Fatal("坑 8：应直跑 ssh.service")
	}
	if !strings.Contains(s, "Port 22") || !strings.Contains(s, "Port 443") {
		t.Fatal("坑 8：sshd 应双听 22/443")
	}
}

// 坑 9：runner 用户 docker 组 + 免密 sudo。
func TestPitfall9RunnerUserSudoDocker(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "groups: [docker]") {
		t.Fatal("坑 9：runner 应在 docker 组")
	}
	if !strings.Contains(s, "sudo: ALL=(ALL) NOPASSWD:ALL") {
		t.Fatal("坑 9：runner 应免密 sudo")
	}
}

// 坑 10：底座五包；ExtraPackages 追加。
func TestPitfall10BasePackages(t *testing.T) {
	s := renderOK(t, validParams())
	for _, pkg := range basePackages {
		if !strings.Contains(s, "  - "+pkg) {
			t.Fatalf("坑 10：缺底座包 %s", pkg)
		}
	}
	p := validParams()
	p.ExtraPackages = []string{"htop"}
	s2 := renderOK(t, p)
	if !strings.Contains(s2, "  - htop") {
		t.Fatal("ExtraPackages 应追加")
	}
}

// 新增：机器侧二进制按版本下载 + sha256 校验 + 失败 exit 1（场景 B 暴露面）。
func TestAgentInstallPinned(t *testing.T) {
	s := renderOK(t, validParams())
	if !strings.Contains(s, "releases/download/v0.1.0/cirunner-linux-amd64") {
		t.Fatal("应按 AgentVersion 下载")
	}
	if !strings.Contains(s, strings.Repeat("a", 64)+"  /usr/local/bin/cirunner") {
		t.Fatal("应带 sha256 校验行")
	}
	if !strings.Contains(s, "| sha256sum -c -") {
		t.Fatal("应执行 sha256sum -c")
	}
}

// 新增：阶段进度标记。
func TestStageMarkers(t *testing.T) {
	s := renderOK(t, validParams())
	for _, stage := range []string{"packages", "agent", "runner-download", "runner-config", "runner-started"} {
		if !strings.Contains(s, stage) {
			t.Fatalf("缺阶段标记 %s", stage)
		}
	}
}

// 新增：baked 分支——/var/lib/cirunner/baked 存在则跳过安装只注册启动。
func TestBakedBranchSkipsInstalls(t *testing.T) {
	s := renderOK(t, validParams())
	// 安装类步骤都要在 BAKED 守卫内
	for _, guarded := range []string{
		"systemctl enable --now docker",
		"releases/download/v0.1.0/cirunner-linux-amd64",
		"actions-runner-linux-x64-${VER}.tar.gz",
	} {
		idx := strings.Index(s, guarded)
		if idx < 0 {
			t.Fatalf("缺步骤 %s", guarded)
		}
		// 向前找最近的 BAKED 判定
		head := s[:idx]
		last := strings.LastIndex(head, "if [ \"$BAKED\" = 0 ]; then")
		if last < 0 {
			t.Fatalf("步骤 %q 不在 BAKED 守卫内（跳过快照分支缺失）", guarded)
		}
	}
	// 注册与启动永远执行（不在守卫内）：守卫结束（fi）之后仍有 config.sh/svc.sh
	if !strings.Contains(s, "--name ci-nms-team2-ci-sgp1-01 --labels ci --unattended") {
		t.Fatal("注册步骤应渲染")
	}
}

// 新增：rpd=2 渲染两个实例（-a/-b 目录与名字）。
func TestTwoInstances(t *testing.T) {
	p := validParams()
	p.RunnersPerDroplet = 2
	s := renderOK(t, p)
	for _, frag := range []string{
		"/opt/actions-runner-a", "/opt/actions-runner-b",
		"--name ci-nms-team2-ci-sgp1-01-a", "--name ci-nms-team2-ci-sgp1-01-b",
		"actions.runner.hoshinojian-nms.ci-nms-team2-ci-sgp1-01-a.service",
		"actions.runner.hoshinojian-nms.ci-nms-team2-ci-sgp1-01-b.service",
	} {
		if !strings.Contains(s, frag) {
			t.Fatalf("两实例渲染缺 %q", frag)
		}
	}
	// rpd=1 不得出现 -a/-b 实例特征
	s1 := renderOK(t, validParams())
	for _, bad := range []string{"actions-runner-a", "actions-runner-b", "-01-a.service", "-01-b.service", "--name " + validParams().RunnerName + "-a"} {
		if strings.Contains(s1, bad) {
			t.Fatalf("单实例渲染不应含两实例特征 %q", bad)
		}
	}
}

// golden 文件：锁定整体产物（防止模板被无意改动）。
func TestGolden(t *testing.T) {
	got := renderOK(t, validParams())
	golden := "testdata/golden.yaml"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(golden, []byte(got), 0o644)
		t.Log("golden 已更新")
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读 golden 失败（先 UPDATE_GOLDEN=1 go test 生成）: %v", err)
	}
	if got != string(want) {
		t.Fatal("产物与 golden 不一致（模板改动需同步更新 golden 并在 PR 说明）")
	}
}

// 参数校验逐项。
func TestValidation(t *testing.T) {
	cases := []struct {
		mutate func(*Params)
		want   string
	}{
		{func(p *Params) { p.PublicKeys = nil }, "PublicKeys"},
		{func(p *Params) { p.PublicKeys = []string{"ssh-ed25519 AAAA \"x\""} }, "公钥"},
		{func(p *Params) { p.RunnerName = "bad name" }, "RunnerName"},
		{func(p *Params) { p.Token = "short" }, "Token"},
		{func(p *Params) { p.RepoURL = "http://x/y/z" }, "RepoURL"},
		{func(p *Params) { p.ServiceRepo = "owner.name" }, "ServiceRepo"},
		{func(p *Params) { p.Labels = nil }, "Labels"},
		{func(p *Params) { p.Labels = []string{"a b"} }, "label"},
		{func(p *Params) { p.AgentVersion = "0.1.0" }, "AgentVersion"},
		{func(p *Params) { p.AgentSHA256 = "xyz" }, "AgentSHA256"},
		{func(p *Params) { p.ExtraPackages = []string{"Bad;Pkg"} }, "ExtraPackages"},
		{func(p *Params) { p.RunnersPerDroplet = 3 }, "RunnersPerDroplet"},
	}
	for _, c := range cases {
		p := validParams()
		c.mutate(&p)
		if _, err := Render(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("校验 %s：want 含 %q，got %v", "case", c.want, err)
		}
	}
	// RunnersPerDroplet=0 默认 1
	p := validParams()
	p.RunnersPerDroplet = 0
	if _, err := Render(p); err != nil {
		t.Fatalf("rpd=0 应默认 1: %v", err)
	}
}
