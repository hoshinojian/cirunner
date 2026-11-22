# cirunner — ciorch 的 runner 机器侧 agent 与 cloud-init 配方

cirunner 提供两样东西，都由 [ciorch](https://github.com/hoshinojian/ciorch) 编排器使用：

1. **`pkg/cloudinit`**：渲染 runner droplet 的 `#cloud-config` user-data（装底座包、下载并
   `sha256sum -c` 校验本仓发布的 agent 二进制、装最新 actions runner、注册、启动）。
   ciorch 经 go module 依赖本包（当前钉 v0.1.3）。
2. **`cirunner` 机器侧二进制**（GitHub Release `v0.1.x`，`cirunner-linux-amd64`）：装在 runner
   机器 `/usr/local/bin/cirunner`，由编排器经 ssh 以 root 调用：

   ```
   cirunner health [--json]                    健康检查（不 OK 退出码 1）
   cirunner diag [--json] [--lines=200]        健康 + 各类日志等诊断材料（不 OK 退出码 1，JSON 仍完整）
   cirunner act <restart-runner|stop-runner|clean-work> [--json]   白名单动作
   ```

调度（观察队列、建删机、事故与诊断）全部在 ciorch。全景说明见 ciorch 仓 `docs/HANDBOOK.md`。

## 沿革：原型调度器（已退役）

2026-11-15 至 2026-11-22，本仓曾是一个在维护者 WSL 上运行的按需 runner 调度原型
（`cirunner -config … status|plan|up|down|daemon`，按 run 粒度观察队列、经 vpsctl 建删机）。
其缺陷与重设计见 ciorch 仓 `docs/REDESIGN.md` §1.4。2026-11-22 nms 的 CI 切换到 ciorch 后，
原型调度代码从本仓删除，**最后包含原型的提交是 `b0225ce`（= v0.1.3）**。
回滚路径（维护者 WSL 上的 `~/.local/bin/cirunner-proto` 与 `systemctl --user` 单元）不依赖本仓
当前代码，步骤见 ciorch `docs/HANDBOOK.md` §10.7。

## 发布

打 `v*` tag 触发 `.github/workflows/release.yml`：`go test -race` 通过后构建
`cirunner-linux-amd64` 并附 sha256。ciorch 配置 `machine_agent.version/sha256` 钉住版本，
cloud-init 下载后校验，校验失败即冷启失败（ciorch 场景 B）。

## cloud-init 固化的坑（改动模板前必读 nms docs/runbooks/gitrunner.md）

- unattended-upgrades 会杀正跑 job → apt 配置禁用
- runner < 2.329.0 注册 404 → 一律 latest release + 版本号格式守卫
- svc.sh install 后 daemon-reload 再 start
- systemd unit 仓名用连字符（owner-name）
- vpsctl `--authorized-keys` 不注入自定义 user-data → `ssh_authorized_keys` 自带
- user-data 纯 ASCII（P75，渲染后强制校验）；文件走 `write_files` block scalar（P70）
- 规格锁 DO c 系专用核（共享核两次击穿 -race 时敏用例）；同规格同区避免
  nms 覆盖率基线重校（决策 #148 先例）

## 布局

```
cmd/cirunner      机器侧 CLI（health/diag/act）
internal/health   健康检查
internal/diag     诊断材料采集
internal/act      白名单动作
internal/sys      系统命令封装（可注入，便于测试）
pkg/cloudinit     cloud-init 渲染（ciorch 依赖的公开 API）
```
