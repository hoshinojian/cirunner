// Package cloudinit 渲染 runner droplet 的 #cloud-config user-data。
//
// 这里固化的每一条都对应 nms 仓 docs/runbooks/gitrunner.md 的装机步骤与已知坑：
//   - 坑#1 unattended-upgrades 自动升级会杀正跑的 job → apt 配置禁用
//   - 坑#2 runner < 2.329.0 注册 404 → 一律从 latest release 下载，并对版本号做格式守卫
//   - 坑#4 svc.sh install 后 systemd 未必立见 unit → daemon-reload 后再 start
//   - unit 仓名用连字符（hoshinojian-nms），写成点号会 Unit not found
//   - vpsctl --authorized-keys 不注入自定义 user-data → ssh_authorized_keys 必须自带
//   - P70/P75：user-data 必须纯 ASCII；文件内容用 write_files block scalar 而非 runcmd 内 printf
package cloudinit

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Params 是渲染入参（全部必填，逐项校验）。
type Params struct {
	RepoURL     string   // https://github.com/owner/name
	ServiceRepo string   // systemd unit 中的仓名段（owner-name，连字符）
	RunnerName  string   // droplet 名 = runner 名
	Labels      []string // 注册 labels（须与 workflow runs-on 匹配）
	Token       string   // GitHub runner registration token（1h 时效，建机时由编排器新铸）
	PublicKeys  []string // ssh 公钥行
}

var (
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9_=-]{16,}$`)
	urlRe   = regexp.MustCompile(`^https://[A-Za-z0-9.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

const tpl = `#cloud-config
# Managed by cirunner. This machine is disposable - do not edit or deploy anything else on it.
# Pitfall fixes encoded below (see nms docs/runbooks/gitrunner.md, known-pitfalls section).
ssh_pwauth: false
ssh_authorized_keys:
{{- range .PublicKeys}}
  - '{{.}}'
{{- end}}

packages:
  - docker.io
  - jq
  - curl
  - build-essential
  - shellcheck

# Pitfall #1: unattended-upgrades restarts services mid-job and kills running CI jobs.
write_files:
  - path: /etc/apt/apt.conf.d/20auto-upgrades
    content: |
      APT::Periodic::Update-Package-Lists "1";
      APT::Periodic::Unattended-Upgrade "0";
  # Rescue access: some network paths filter inbound 22 to DO sgp1 (2026-11-15
  # smoke: fresh IP unreachable on 22/ICMP from the ops host). sshd also listens
  # on 443, same pattern as nms test nodes (sshd dual-port in nms pitfalls).
  - path: /etc/ssh/sshd_config.d/99-cirunner-ports.conf
    content: |
      Port 22
      Port 443

users:
  - name: runner
    groups: [docker]
    shell: /bin/bash
    # gitleaks/sqlc CI jobs need "sudo install" of pinned binaries.
    sudo: ALL=(ALL) NOPASSWD:ALL

runcmd:
  - systemctl enable --now docker
  - |
    # Fix root access early, then make sshd listen on both ports.
    # DO's ubuntu image marks the (unknown, random) root password expired, which
    # blocks even pubkey logins with "password change required"; clear the expiry
    # so key-only auth works (2026-11-15 smoke finding). Ubuntu 24.04 also has the
    # ssh.socket activation trap: disable it and run ssh.service directly.
    set -x
    chage -d "$(date +%F)" root
    systemctl disable --now ssh.socket || true
    systemctl enable ssh.service || true
    systemctl restart ssh
  - |
    #!/bin/sh
    # Install GitHub Actions runner from the LATEST release (>= 2.329.0 mandatory:
    # older tarballs 404 on registration). Machine is disposable, so no upgrade path needed.
    set -eux
    mkdir -p /opt/actions-runner
    cd /opt/actions-runner
    VER=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name | ltrimstr("v")')
    case "$VER" in
      ''|*[!0-9.]*) echo "bad runner version: $VER" >&2; exit 1;;
    esac
    curl -fsSL -o runner.tgz "https://github.com/actions/runner/releases/download/v${VER}/actions-runner-linux-x64-${VER}.tar.gz"
    tar xzf runner.tgz
    rm -f runner.tgz
    ./bin/installdependencies.sh
    # config.sh runs as the runner user; /opt must be writable by it.
    chown -R runner: /opt/actions-runner
    sudo -u runner ./config.sh --url {{.RepoURL}} --token {{.Token}} --name {{.RunnerName}} --labels {{.Labels}} --unattended
    ./svc.sh install runner
    # Pitfall #4: systemd may not see the fresh unit until daemon-reload.
    systemctl daemon-reload
    systemctl start actions.runner.{{.ServiceRepo}}.{{.RunnerName}}.service
`

// Render 生成 cloud-init 字节流。参数逐项校验，产物强制纯 ASCII（P75）。
func Render(p Params) ([]byte, error) {
	if len(p.PublicKeys) == 0 {
		return nil, fmt.Errorf("PublicKeys 不能为空（vpsctl --authorized-keys 不注入自定义 user-data）")
	}
	for _, k := range p.PublicKeys {
		if strings.ContainsAny(k, "'\"\n") {
			return nil, fmt.Errorf("公钥 %q 含引号/换行等危险字符", k)
		}
	}
	if !nameRe.MatchString(p.RunnerName) {
		return nil, fmt.Errorf("RunnerName %q 非法（须字母数字.-）", p.RunnerName)
	}
	if !tokenRe.MatchString(p.Token) {
		return nil, fmt.Errorf("Token 非法（须 >=16 位 token 字符集）")
	}
	if !urlRe.MatchString(p.RepoURL) {
		return nil, fmt.Errorf("RepoURL %q 须为 https://host/owner/name", p.RepoURL)
	}
	if !nameRe.MatchString(p.ServiceRepo) || strings.Contains(p.ServiceRepo, "/") {
		return nil, fmt.Errorf("ServiceRepo %q 非法（owner-name 连字符形式）", p.ServiceRepo)
	}
	if len(p.Labels) == 0 {
		return nil, fmt.Errorf("Labels 不能为空")
	}
	for _, l := range p.Labels {
		if !labelRe.MatchString(l) {
			return nil, fmt.Errorf("label %q 含非法字符", l)
		}
	}

	t, err := template.New("cloudinit").Parse(tpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	data := struct {
		Params
		Labels string
	}{p, strings.Join(p.Labels, ",")}
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if err := ensureASCII(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ensureASCII 强制 P75：DO user-data 管道会把任何 >0x7F 字节毁成 C1 控制字符。
func ensureASCII(b []byte) error {
	for i, c := range b {
		if c > 0x7F {
			return fmt.Errorf("user-data 含非 ASCII 字节 0x%02X @%d（P75）", c, i)
		}
	}
	return nil
}
