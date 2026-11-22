// Package cloudinit 是 cirunner 的公开渲染 API（REDESIGN §5.3）：ciorch 经
// go module 依赖本包渲染 runner droplet 的 #cloud-config user-data。
//
// 自原型的 internal/cloudinit 迁出并扩展（原型调度器已于 2026-11-22 退役，
// internal 副本随之删除）。固化的每一条坑对应 nms docs/runbooks/gitrunner.md 已知坑（括号内编号）：
//
//	（1）unattended-upgrades 自动升级杀正跑 job → apt 配置禁用
//	（2）runner < 2.329.0 注册 404 → 一律 latest + 版本号格式守卫
//	（3）svc.sh install 后 systemd 未必立见 unit → daemon-reload 后再 start
//	（4）unit 仓名用连字符，点号会 Unit not found
//	（5）vpsctl --authorized-keys 不注入自定义 user-data → 模板自带公钥
//	（6）P70/P75：user-data 纯 ASCII；文件内容走 write_files block scalar
//	（7）DO 镜像 root 密码过期挡公钥登录 → chage 解除；ssh_pwauth: false
//	（8）Ubuntu 24.04 ssh.socket 激活陷阱 → 禁 socket 直跑 service；sshd
//	     同时听 22 和 443（救援通道）
//	（9）runner 用户进 docker 组、免密 sudo（gitleaks/sqlc 要 sudo install）
//	（10）底座包 docker.io/jq/curl/build-essential/shellcheck（可配 extra）
//
// 新增（§5.3）：机器侧二进制按版本下载并 sha256 校验（失败 exit 1，让冷启
// 失败暴露为场景 B）；阶段进度标记 /var/lib/cirunner/stage；baked 快照分支
// （/var/lib/cirunner/baked 存在则跳过安装只注册启动）；runners_per_droplet=2
// 渲染两个实例（/opt/actions-runner-a、-b，名 -a/-b）。
package cloudinit

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Params 是渲染入参（全部必填或带默认，逐项校验）。
type Params struct {
	RepoURL           string   // https://github.com/owner/name
	ServiceRepo       string   // systemd unit 中的仓名段（owner-name，连字符，坑 4）
	RunnerName        string   // droplet 名 = runner 名（rpd=2 时加 -a/-b 后缀）
	Labels            []string // 注册 labels（须与 workflow runs-on 匹配）
	Token             string   // GitHub runner registration token（1h 时效，建机时新铸）
	PublicKeys        []string // ssh 公钥行（编排器 + 维护者）
	AgentVersion      string   // 机器侧 cirunner 版本 tag（如 v0.1.0）
	AgentSHA256       string   // release 资产 sha256（64 位 hex）
	ExtraPackages     []string // 额外包（默认底座五包之外）
	RunnersPerDroplet int      // 1 或 2（2 为实验特性，REDESIGN §3.6.4）
}

// instance 是一个 runner 实例（rpd=2 时两个）。
type instance struct {
	Dir  string // 安装目录
	Name string // runner 名
}

var (
	nameRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	serviceRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`) // 坑 4：点号会 Unit not found
	tokenRe       = regexp.MustCompile(`^[A-Za-z0-9_=-]{16,}$`)
	urlRe         = regexp.MustCompile(`^https://[A-Za-z0-9.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	labelRe       = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	verRe         = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Re      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	pkgRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
)

// maxUserDataBytes 是 DO user-data 上限（§5.3：产物 ≤64KB）。
const maxUserDataBytes = 64 << 10

// basePackages 是底座五包（坑 10；nms CI 需要全部）。
var basePackages = []string{"docker.io", "jq", "curl", "build-essential", "shellcheck"}

const tpl = `#cloud-config
# Managed by cirunner. This machine is disposable - do not edit or deploy anything else on it.
# Pitfall fixes encoded below are numbered as in nms docs/runbooks/gitrunner.md.
ssh_pwauth: false
ssh_authorized_keys:
{{- range .PublicKeys}}
  - '{{.}}'
{{- end}}

packages:
{{- range .Packages}}
  - {{.}}
{{- end}}

# Pitfall (1): unattended-upgrades restarts services mid-job and kills running CI jobs.
write_files:
  - path: /etc/apt/apt.conf.d/20auto-upgrades
    content: |
      APT::Periodic::Update-Package-Lists "1";
      APT::Periodic::Unattended-Upgrade "0";
  # Rescue access (8): some network paths filter inbound 22; sshd also listens on 443.
  - path: /etc/ssh/sshd_config.d/99-cirunner-ports.conf
    content: |
      Port 22
      Port 443
  # Stage markers for diagnostics: packages -> agent -> runner-download ->
  # runner-config -> runner-started (health/diag read this to see where boot stalled).
  - path: /var/lib/cirunner/stage
    content: |
      packages

users:
  - name: runner
    groups: [docker]
    shell: /bin/bash
    # Pitfall (9): gitleaks/sqlc CI jobs need "sudo install" of pinned binaries.
    sudo: ALL=(ALL) NOPASSWD:ALL

runcmd:
  - |
    #!/bin/sh
    set -eux
    BAKED=0; [ -f /var/lib/cirunner/baked ] && BAKED=1
    # Baked snapshot images already installed everything except registration/start.
    if [ "$BAKED" = 0 ]; then
      systemctl enable --now docker
    fi
  - |
    # Fix root access early (7): DO's ubuntu image marks the root password expired
    # which blocks even pubkey logins; clear it for key-only auth.
    # Pitfall (8): Ubuntu 24.04 ssh.socket activation trap - disable socket, run
    # ssh.service directly, and reload to pick up the 22+443 listen config.
    set -x
    chage -d "$(date +%F)" root
    systemctl disable --now ssh.socket || true
    systemctl enable ssh.service || true
    systemctl restart ssh
  - |
    #!/bin/sh
    # Machine-side agent binary (cirunner health/diag/act), pinned by version and
    # sha256 (REDESIGN 5.3). A bad download must fail the boot (exit 1) so cold
    # start surfaces as scenario B instead of a half-healthy machine.
    set -eux
    BAKED=0; [ -f /var/lib/cirunner/baked ] && BAKED=1
    if [ "$BAKED" = 0 ]; then
      curl -fsSL -o /usr/local/bin/cirunner https://github.com/hoshinojian/cirunner/releases/download/{{.AgentVersion}}/cirunner-linux-amd64
      echo '{{.AgentSHA256}}  /usr/local/bin/cirunner' | sha256sum -c -
      chmod 0755 /usr/local/bin/cirunner
    fi
    echo agent > /var/lib/cirunner/stage
  - |
    #!/bin/sh
    # Install GitHub Actions runner from the LATEST release (2): >= 2.329.0
    # mandatory, older tarballs 404 on registration. Disposable machine, no
    # upgrade path needed.
    set -eux
    BAKED=0; [ -f /var/lib/cirunner/baked ] && BAKED=1
    if [ "$BAKED" = 0 ]; then
{{- range .Instances}}
      mkdir -p {{.Dir}}
      cd {{.Dir}}
      VER=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name | ltrimstr("v")')
      case "$VER" in
        ''|*[!0-9.]*) echo "bad runner version: $VER" >&2; exit 1;;
      esac
      curl -fsSL -o runner.tgz "https://github.com/actions/runner/releases/download/v${VER}/actions-runner-linux-x64-${VER}.tar.gz"
      tar xzf runner.tgz
      rm -f runner.tgz
      ./bin/installdependencies.sh
{{- end}}
    fi
    echo runner-download > /var/lib/cirunner/stage
  - |
    #!/bin/sh
    # Register each runner instance (the only non-baked step besides start).
    set -eux
    echo runner-config > /var/lib/cirunner/stage
{{- range .Instances}}
    chown -R runner: {{.Dir}}
    sudo -u runner {{.Dir}}/config.sh --url {{$.RepoURL}} --token {{$.Token}} --name {{.Name}} --labels {{$.Labels}} --unattended
{{- end}}
  - |
    #!/bin/sh
    # Install services (3): svc.sh install then daemon-reload BEFORE start, or
    # systemd may not see the fresh unit. Unit repo segment is hyphenated (4).
    set -eux
{{- range .Instances}}
    cd {{.Dir}}
    ./svc.sh install runner
    systemctl daemon-reload
    systemctl start actions.runner.{{$.ServiceRepo}}.{{.Name}}.service
{{- end}}
    echo runner-started > /var/lib/cirunner/stage
`

// Render 生成 cloud-init 字节流。参数逐项校验，产物强制纯 ASCII（坑 6/P75）
// 且 ≤ 64KB（DO user-data 上限）。
func Render(p Params) ([]byte, error) {
	if p.RunnersPerDroplet == 0 {
		p.RunnersPerDroplet = 1
	}
	if p.RunnersPerDroplet != 1 && p.RunnersPerDroplet != 2 {
		return nil, fmt.Errorf("RunnersPerDroplet 须为 1 或 2（2 为实验特性），got %d", p.RunnersPerDroplet)
	}
	if len(p.PublicKeys) == 0 {
		return nil, fmt.Errorf("PublicKeys 不能为空（vpsctl --authorized-keys 不注入自定义 user-data，坑 5）")
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
	if !serviceRepoRe.MatchString(p.ServiceRepo) {
		return nil, fmt.Errorf("ServiceRepo %q 非法（owner-name 连字符形式，坑 4：点号会 Unit not found）", p.ServiceRepo)
	}
	if len(p.Labels) == 0 {
		return nil, fmt.Errorf("Labels 不能为空")
	}
	for _, l := range p.Labels {
		if !labelRe.MatchString(l) {
			return nil, fmt.Errorf("label %q 含非法字符", l)
		}
	}
	if !verRe.MatchString(p.AgentVersion) {
		return nil, fmt.Errorf("AgentVersion %q 须为 vX.Y.Z 形式", p.AgentVersion)
	}
	if !sha256Re.MatchString(p.AgentSHA256) {
		return nil, fmt.Errorf("AgentSHA256 %q 须为 64 位小写 hex", p.AgentSHA256)
	}
	for _, pkg := range p.ExtraPackages {
		if !pkgRe.MatchString(pkg) {
			return nil, fmt.Errorf("ExtraPackages %q 非法", pkg)
		}
	}

	// 实例列表（rpd=2 → -a/-b；rpd=1 沿用无后缀目录与名字）
	instances := []instance{{Dir: "/opt/actions-runner", Name: p.RunnerName}}
	if p.RunnersPerDroplet == 2 {
		instances = []instance{
			{Dir: "/opt/actions-runner-a", Name: p.RunnerName + "-a"},
			{Dir: "/opt/actions-runner-b", Name: p.RunnerName + "-b"},
		}
	}
	packages := append([]string{}, basePackages...)
	packages = append(packages, p.ExtraPackages...)

	t, err := template.New("cloudinit").Parse(tpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	data := struct {
		Params
		Packages  []string
		Labels    string
		Instances []instance
	}{p, packages, strings.Join(p.Labels, ","), instances}
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if err := ensureASCII(out); err != nil {
		return nil, err
	}
	if len(out) > maxUserDataBytes {
		return nil, fmt.Errorf("user-data %d 字节超 DO 上限 %d（精简 ExtraPackages）", len(out), maxUserDataBytes)
	}
	return out, nil
}

// ensureASCII 强制坑 6/P75：DO user-data 管道会把任何 >0x7F 字节毁成 C1 控制字符。
func ensureASCII(b []byte) error {
	for i, c := range b {
		if c > 0x7F {
			return fmt.Errorf("user-data 含非 ASCII 字节 0x%02X @%d（P75）", c, i)
		}
	}
	return nil
}
