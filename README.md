# cirunner — 按需 GitHub Actions 自建 runner 池

守护进程盯各配置仓的 CI 队列：有排队 run 就建机装 runner，空闲就删机。
项目无关——nms 只是第一个租户，其他项目在配置里加一条 `repos` 即可。
动机：DO 关机不停止计费，只有删机省钱（$94/月常驻 → 按需约 $0.14/h）。

## 架构

```
cirunner daemon（任意常驻 Linux，先跑 WSL2）
  每 60s 每仓：gh api（排队/runner 状态）+ vpsctl list --tag（droplet）
  → decide（纯函数）→ 动作：
      建机   有排队且无 droplet（铸 1h token → cloud-init → vpsctl create）
      删机   无排队且 runner 空闲持续 20min
      换机   P139 判据：排队最老 >15min 且 busy=false 连续两采样
      冷启超时 建机后 15min runner 未上线 → 删了重建；连续 2 次失败转告警
  约束：全局 max_droplets（默认 2）、每仓 max_runners（默认 1）
```

- **会话式常驻 runner**（droplet 内 systemd 服务，非 `--ephemeral`）：nms 七项
  checks 是 7 个并行 job，一单一注会让每个 job 多付一次冷启动；单机串行 ≈23min
  与现状一致。disposable 的是机器。
- **判据只用 GitHub 侧信号**（pitfalls P139 教训：机器侧 journal 静默不可作依据，
  test job 跑起来的 ~19 分钟里本来就安静）。
- 机器侧不存任何凭据/不自毁：回收统一由本地编排执行（守 nms runbook 安全守则）。

## 快速开始

```bash
cp configs/cirunner.example.json ~/.config/cirunner/cirunner.json  # 按需改
make build
bin/cirunner status          # 看队列/runner/droplet
bin/cirunner plan            # 看本轮会做什么（不执行）
bin/cirunner up nms          # 手动建一台（冷启动约 4~6 分钟）
bin/cirunner down nms        # 手动回收（有排队时拒绝，-force 覆盖）
systemctl --user enable --now cirunner   # 守护托管（拷 systemd/cirunner.service 到 ~/.config/systemd/user/）
```

## 前置条件

1. `vpsctl`（子进程调用，不改它）：本仓按 commit `c55ed90`（sha256
   `a19e6a0a40b359387f2683b3bb11c998e5b75837912ca298737f44c58234a2a3`）验证过。
2. vpsctl 账号配置里为 CI 加一个**无密码**账号条目（如 `team2-ci`，复用 DO token）：
   vpsctl 守卫规定带 `ssh_password` 的账号与 `--user-data` 互斥；passwordless +
   user-data 走 D3 分叉合法。CI 一次性机器只走公钥，密码不落机。
3. `gh` 已登录且对租户仓有 admin（铸 registration token 需要）。
4. 租户仓 workflow 的 `runs-on` labels 与配置 `labels` 匹配（如 `[self-hosted, ci]`
   ↔ `["ci"]`；`self-hosted` 是注册时自动带的）。

## cloud-init 固化的坑（改动模板前必读 nms docs/runbooks/gitrunner.md）

- unattended-upgrades 会杀正跑 job → apt 配置禁用
- runner < 2.329.0 注册 404 → 一律 latest release + 版本号格式守卫
- svc.sh install 后 daemon-reload 再 start
- systemd unit 仓名用连字符（owner-name）
- vpsctl `--authorized-keys` 不注入自定义 user-data → `ssh_authorized_keys` 自带
- user-data 纯 ASCII（P75，渲染后强制校验）；文件走 `write_files` block scalar（P70）
- 规格锁 DO c 系专用核（共享核两次击穿 -race 时敏用例）；同规格同区避免
  nms 覆盖率基线重校（决策 #148 先例）

## 已知约束

- 冷启动 4~6 分钟：空闲后首个 PR 多等这一窗口（可做预装 snapshot 优化，未做）。
- 守护不在线则 CI 不会自动起（WSL2 睡眠/关机）：迁移到任意常驻 Linux 即可，
  守护本身不跑 CI、不占资源，$4~6/月微型机足够。
- 判据按 **run** 而非 job 粒度：排队 run 数即队列信号，nms 七项串行由单 runner 消化。

## 布局

```
cmd/cirunner          CLI（status/plan/up/down/daemon）
internal/config       JSON 配置加载校验
internal/decide       纯决策（无 IO，全分支单测）
internal/cloudinit    模板渲染 + ASCII 校验
internal/githubapi    gh api 子进程封装
internal/vpsctlapi    vpsctl 子进程封装（create/list/delete）
internal/engine       编排粘合（观察→决策→执行，依赖注入可测）
```
