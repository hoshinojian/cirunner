// cirunner — runner 机器侧 agent（由 ciorch 编排器经 ssh 调用；cloud-init
// 下载并校验 sha256 后装到 /usr/local/bin/cirunner）。
//
// 用法（需 root）:
//
//	cirunner health [--json]                          runner/docker/磁盘等健康检查（不 OK 退出码 1）
//	cirunner diag [--json] [--lines=200]              健康 + 日志等诊断材料（不 OK 退出码 1，输出仍完整）
//	cirunner act <restart-runner|stop-runner|clean-work> [--json]   白名单动作
//
// 调度（观察队列、建删机）在 ciorch 仓；本仓原型调度器已于 2026-11-22 退役
// （最后版本见提交 b0225ce），cloud-init 渲染库见 pkg/cloudinit。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if handled, code := runMachineCommand(os.Args[1], os.Args[2:]); handled {
		os.Exit(code)
	}
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprint(os.Stderr, `cirunner — runner 机器侧 agent（需 root）

用法:
  cirunner health [--json]
  cirunner diag [--json] [--lines=200]
  cirunner act <restart-runner|stop-runner|clean-work> [--json]
`)
}

// ci acceptance probe 20261123（验收用，不合并）
