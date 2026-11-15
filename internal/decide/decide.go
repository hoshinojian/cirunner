// Package decide 是 cirunner 的纯决策核心：输入一轮"世界采样"（GitHub 侧队列/runner
// 状态 + DO 侧 droplet 清单）与上轮遗留状态，输出本轮动作与新状态。
// 不做任何 IO，全部逻辑可单测。
//
// 判定口径全部来自 nms 仓踩坑实录（docs/runbooks/gitrunner.md 已知坑 + pitfalls P139）：
//   - 就绪/空闲/停摆判据只用 GitHub 侧信号，不依赖机器侧日志（P139）
//   - 排队很老但 runner busy 属正常串行消化（nms 单 run 七项串行 ≈23 分钟）
package decide

import "time"

// Droplet 是 vpsctl 侧观察到的、属于本仓的 cirunner droplet。
type Droplet struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Runner 是 GitHub 侧观察到的 self-hosted runner 注册。
type Runner struct {
	ID     int64
	Name   string
	Status string // "online" | "offline"
	Busy   bool
}

// Obs 是一轮采样（全部 GitHub/DO 侧事实，按仓过滤后传入）。
type Obs struct {
	Now             time.Time
	QueuedRuns      int           // 排队中的 workflow run 数
	OldestQueuedAge time.Duration // 最老排队 run 的年龄；QueuedRuns==0 时无意义
	Droplets        []Droplet
	Runners         []Runner
}

// State 是守护进程跨轮携带的记忆（每仓一份）。
type State struct {
	IdleSince      time.Time // 观察到"无队列且 runner 空闲"的起点；零值=不在空闲计时
	PendingSince   time.Time // "有 droplet 但 runner 未 online"的冷启起点；零值=无冷启在途
	StallSamples   int       // 连续停摆信号采样计数
	ColdStartFails int       // 连续冷启超时次数
}

// Cfg 是决策参数（零值字段取默认）。
type Cfg struct {
	NamePrefix         string        // 本编排器拥有的 runner/droplet 名前缀，默认 "cirunner"
	MaxRunners         int           // 每仓 droplet 上限，默认 1
	IdleTimeout        time.Duration // 空闲回收阈值，默认 20m
	ColdStartTimeout   time.Duration // 冷启（建机到 runner online）阈值，默认 15m
	StallQueuedAge     time.Duration // 停摆判据的排队年龄阈值，默认 15m
	StallSamplesNeeded int           // 停摆确认所需连续采样数，默认 2（P139 口径）
	MaxColdStartFails  int           // 连续冷启失败上限，超过则告警停建，默认 2
}

// ActionKind 是动作类型。
type ActionKind string

const (
	ActCreate             ActionKind = "create"              // 建机（调用方负责铸 token + 渲染 cloud-init）
	ActDeleteDroplet      ActionKind = "delete-droplet"      // 删机（下轮按需自然重建）
	ActRemoveRegistration ActionKind = "remove-registration" // 清理 GitHub 侧残留注册
	ActAlert              ActionKind = "alert"               // 告警（需人工介入）
)

// Action 是一轮决策产生的单个动作。
type Action struct {
	Kind       ActionKind
	DropletID  string
	RunnerName string
	Reason     string
}

const (
	defaultNamePrefix  = "cirunner"
	defaultIdleTimeout = 20 * time.Minute
	defaultColdStart   = 15 * time.Minute
	defaultStallAge    = 15 * time.Minute
	defaultMaxFails    = 2
)

// Decide 执行一轮决策。纯函数：相同 (cfg, state, obs) 恒定输出 (actions, newState)。
func Decide(c Cfg, st State, obs Obs) ([]Action, State) {
	if c.NamePrefix == "" {
		c.NamePrefix = defaultNamePrefix
	}
	if c.MaxRunners == 0 {
		c.MaxRunners = 1
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.ColdStartTimeout == 0 {
		c.ColdStartTimeout = defaultColdStart
	}
	if c.StallQueuedAge == 0 {
		c.StallQueuedAge = defaultStallAge
	}
	if c.StallSamplesNeeded == 0 {
		c.StallSamplesNeeded = 2
	}
	if c.MaxColdStartFails == 0 {
		c.MaxColdStartFails = defaultMaxFails
	}

	var acts []Action
	emit := func(k ActionKind, id string, name, reason string) {
		acts = append(acts, Action{Kind: k, DropletID: id, RunnerName: name, Reason: reason})
	}

	dropletNames := make(map[string]bool, len(obs.Droplets))
	for _, d := range obs.Droplets {
		dropletNames[d.Name] = true
	}

	// 1) 清理本前缀的 offline 残留注册（机器已删、心跳失效后的尾巴）。
	//    online 但无 droplet 的注册先不动（可能是刚删机后心跳尚未失效）。
	//    前缀不匹配的外部 runner（如 pilot 期旧手工机）绝不触碰。
	for _, r := range obs.Runners {
		if !hasPrefix(r.Name, c.NamePrefix) {
			continue
		}
		if r.Status == "offline" && !dropletNames[r.Name] {
			emit(ActRemoveRegistration, "", r.Name, "offline 残留注册，机器已不存在")
		}
	}

	// 与现存 droplet 名字对上的 online runner = 活跃执行面。
	var active []Runner
	for _, r := range obs.Runners {
		if r.Status == "online" && dropletNames[r.Name] {
			active = append(active, r)
		}
	}

	deleteAll := func(reason string) {
		for _, d := range obs.Droplets {
			emit(ActDeleteDroplet, d.ID, d.Name, reason)
		}
	}

	// 2) 无 droplet：按需建机或告警。
	if len(obs.Droplets) == 0 {
		st.PendingSince, st.IdleSince, st.StallSamples = time.Time{}, time.Time{}, 0
		if obs.QueuedRuns > 0 {
			if st.ColdStartFails >= c.MaxColdStartFails {
				emit(ActAlert, "", "", "连续冷启失败已达上限，停止自动建机，需人工排查 cloud-init/网络")
			} else {
				emit(ActCreate, "", "", "有排队 run 且无 runner")
			}
		}
		return acts, st
	}

	// 3) 有 droplet 无 online runner：冷启窗口。
	if len(active) == 0 {
		st.IdleSince, st.StallSamples = time.Time{}, 0
		if st.PendingSince.IsZero() {
			st.PendingSince = obs.Now
			return acts, st
		}
		if obs.Now.Sub(st.PendingSince) > c.ColdStartTimeout {
			deleteAll("冷启超时：droplet 存在但 runner 未上线")
			st.PendingSince = time.Time{}
			st.ColdStartFails++
		}
		return acts, st
	}

	// 4) 有活跃 runner：正常运转，先重置冷启记忆。
	st.PendingSince = time.Time{}
	st.ColdStartFails = 0

	// 5) 停摆判定（P139）：排队很老 + online runner 全部不接活，连续两轮采样确认。
	stalled := obs.QueuedRuns > 0 && obs.OldestQueuedAge > c.StallQueuedAge && allIdle(active)
	if stalled {
		st.StallSamples++
		if st.StallSamples >= c.StallSamplesNeeded {
			deleteAll("停摆（P139 口径）：排队超龄且 runner 不接活，删机换新")
			st.StallSamples, st.IdleSince = 0, time.Time{}
			return acts, st
		}
	} else {
		st.StallSamples = 0
	}

	// 6) 空闲回收：无队列且全部空闲，持续满 IdleTimeout 才删。
	if obs.QueuedRuns == 0 && allIdle(active) {
		if st.IdleSince.IsZero() {
			st.IdleSince = obs.Now
		} else if obs.Now.Sub(st.IdleSince) > c.IdleTimeout {
			deleteAll("空闲回收：无排队且 runner 空闲超时")
			st.IdleSince = time.Time{}
		}
	} else {
		st.IdleSince = time.Time{}
	}

	// 7) 扩容：有排队且未达每仓上限（冷启在途时不叠加建，避免同仓并发冷启）。
	if obs.QueuedRuns > 0 && len(obs.Droplets) < c.MaxRunners && len(active) > 0 {
		emit(ActCreate, "", "", "有排队 run 且 runner 全忙，未达每仓上限")
	}

	return acts, st
}

func allIdle(rs []Runner) bool {
	for _, r := range rs {
		if r.Busy {
			return false
		}
	}
	return true
}

func hasPrefix(name, prefix string) bool {
	if len(name) <= len(prefix) {
		return false
	}
	return name[:len(prefix)] == prefix && (name[len(prefix)] == '-' || name[len(prefix)] == '_')
}
