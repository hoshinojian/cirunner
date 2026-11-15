package cloudinit

import (
	"strings"
	"testing"
)

func params() Params {
	return Params{
		RepoURL:     "https://github.com/hoshinojian/nms",
		ServiceRepo: "hoshinojian-nms",
		RunnerName:  "cirunner-team2-sgp1-01",
		Labels:      []string{"ci"},
		Token:       "AGB2ZQKHL4TGHTRSEXAMPLE",
		PublicKeys:  []string{"ssh-ed25519 AAAAC3NzaC1lZDI1AAAAAAAIAAAAQAtestexamplekey user@host"},
	}
}

func TestRenderContainsPitfallFixes(t *testing.T) {
	out, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	must := []string{
		// 坑#1：禁 unattended-upgrades（自动升级会杀正跑 job）
		`APT::Periodic::Unattended-Upgrade "0";`,
		// DO 镜像 root 密码过期会挡公钥登录（2026-11-15 冒烟实录）
		`chage -d "$(date +%F)" root`,
		"ssh_pwauth: false",
		// 救援通道：22 被路径过滤时仍可 443 进机（2026-11-15 冒烟实录）
		"Port 22",
		"Port 443",
		"systemctl disable --now ssh.socket",
		// 坑#2：runner 用 latest（≥2.329.0 强制，旧版注册 404）
		"api.github.com/repos/actions/runner/releases/latest",
		// 坑#4：svc install 后 daemon-reload
		"systemctl daemon-reload",
		// 坑：unit 仓名是连字符
		"actions.runner.hoshinojian-nms.cirunner-team2-sgp1-01.service",
		// 注册参数
		"--url https://github.com/hoshinojian/nms",
		"--name cirunner-team2-sgp1-01",
		"--labels ci",
		"--token AGB2ZQKHL4TGHTRSEXAMPLE",
		// 底座软件（runbook 装机步骤 2）
		"docker.io", "jq", "curl", "build-essential", "shellcheck",
		// gitleaks/sqlc job 需要 sudo install
		"NOPASSWD:ALL",
		// /opt 属 root，config.sh 以 runner 用户跑需要 chown
		"chown -R runner: /opt/actions-runner",
		// ssh 公钥必须自带（vpsctl --authorized-keys 不进自定义 user-data）
		"ssh_authorized_keys",
		"ssh-ed25519 AAAAC3NzaC1lZDI1AAAAAAAIAAAAQAtestexamplekey user@host",
		// root 解包后以 runner 用户注册
		"sudo -u runner ./config.sh",
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("cloud-init 缺少关键内容: %q", m)
		}
	}
}

func TestRenderRejectsBadParams(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Params)
	}{
		{"runner 名带空格", func(p *Params) { p.RunnerName = "bad name" }},
		{"runner 名带斜杠", func(p *Params) { p.RunnerName = "a/b" }},
		{"token 为空", func(p *Params) { p.Token = "" }},
		{"token 含非法字符", func(p *Params) { p.Token = "abc$def" }},
		{"label 含空格", func(p *Params) { p.Labels = []string{"bad label"} }},
		{"repo URL 非 https", func(p *Params) { p.RepoURL = "http://x/y" }},
		{"service repo 带斜杠", func(p *Params) { p.ServiceRepo = "a/b" }},
		{"公钥含单引号注入", func(p *Params) { p.PublicKeys = []string{"ssh-ed25519 AAA 'evil"} }},
	}
	for _, c := range cases {
		p := params()
		c.mut(&p)
		if _, err := Render(p); err == nil {
			t.Errorf("%s: 应报错", c.name)
		}
	}
}

func TestRenderMultipleKeys(t *testing.T) {
	p := params()
	p.PublicKeys = append(p.PublicKeys, "ssh-ed25519 BBBBsecondkey user2@host")
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "BBBBsecondkey") {
		t.Fatal("第二把公钥未渲染")
	}
}

func TestRenderMultipleLabels(t *testing.T) {
	p := params()
	p.Labels = []string{"ci", "heavy"}
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "--labels ci,heavy") {
		t.Fatal("多 label 应逗号连接")
	}
}

func TestEnsureASCIIRejectsNonASCII(t *testing.T) {
	if err := ensureASCII([]byte("plain ascii")); err != nil {
		t.Fatal(err)
	}
	if err := ensureASCII([]byte("含中文")); err == nil {
		t.Fatal("非 ASCII 应报错（P75：DO 管道会把非 ASCII 毁成 C1 控制字符）")
	}
	if err := ensureASCII([]byte("trailing \xc3\xa9")); err == nil {
		t.Fatal("非 ASCII 字节应报错")
	}
}

func TestOutputIsASCIIClean(t *testing.T) {
	out, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureASCII(out); err != nil {
		t.Fatalf("渲染产物必须纯 ASCII: %v", err)
	}
}

func TestVersionSanityGuard(t *testing.T) {
	out, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	// latest 版本号解析失败（如 API 限流返回空）必须硬失败，不能拿空 URL 去 curl
	if !strings.Contains(string(out), "bad runner version") {
		t.Fatal("cloud-init 应包含版本号合法性守卫")
	}
}
