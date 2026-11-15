package decide

import (
	"testing"
	"time"
)

var base = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func cfg() Cfg { return Cfg{NamePrefix: "cirunner"} }

func obs() Obs { return Obs{Now: base} }

func droplet(id int64, name string) Droplet {
	return Droplet{ID: id, Name: name, CreatedAt: base.Add(-10 * time.Minute)}
}

func runner(name string, busy bool) Runner {
	return Runner{ID: 99, Name: name, Status: "online", Busy: busy}
}

func hasCreate(acts []Action) bool {
	for _, a := range acts {
		if a.Kind == ActCreate {
			return true
		}
	}
	return false
}

func hasDelete(acts []Action) bool {
	for _, a := range acts {
		if a.Kind == ActDeleteDroplet {
			return true
		}
	}
	return false
}

func hasRemoveReg(acts []Action, name string) bool {
	for _, a := range acts {
		if a.Kind == ActRemoveRegistration && a.RunnerName == name {
			return true
		}
	}
	return false
}

func hasAlert(acts []Action) bool {
	for _, a := range acts {
		if a.Kind == ActAlert {
			return true
		}
	}
	return false
}

func TestDefaultsApplied(t *testing.T) {
	_, _ = Decide(Cfg{NamePrefix: "cirunner"}, State{}, Obs{Now: base})
	// 走一遍不 panic 即可；具体默认值在下面用例里断言行为
}

func TestCreateWhenQueuedAndEmpty(t *testing.T) {
	o := obs()
	o.QueuedRuns = 1
	o.OldestQueuedAge = 2 * time.Minute
	acts, st := Decide(cfg(), State{}, o)
	if !hasCreate(acts) {
		t.Fatalf("queued>0 且无 droplet 应建机：%v", acts)
	}
	if !st.PendingSince.IsZero() {
		t.Fatalf("建机动作时不应预置 PendingSince（下轮采样才记）")
	}
}

func TestNoCreateWithoutQueue(t *testing.T) {
	acts, _ := Decide(cfg(), State{}, obs())
	if hasCreate(acts) {
		t.Fatalf("无队列不应建机：%v", acts)
	}
}

func TestColdStartPendingFirstSampleNoDelete(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.QueuedRuns = 1
	acts, st := Decide(cfg(), State{}, o)
	if hasDelete(acts) || hasCreate(acts) {
		t.Fatalf("冷启首个采样不应删也不应再建：%v", acts)
	}
	if st.PendingSince != base {
		t.Fatalf("PendingSince 应记为本次采样时间，got %v", st.PendingSince)
	}
}

func TestColdStartTimeoutDeletesAndCountsFail(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.QueuedRuns = 1
	st := State{PendingSince: base.Add(-16 * time.Minute)}
	acts, st2 := Decide(cfg(), st, o)
	if !hasDelete(acts) {
		t.Fatalf("冷启超时应删机：%v", acts)
	}
	if st2.ColdStartFails != 1 {
		t.Fatalf("ColdStartFails 应计 1，got %d", st2.ColdStartFails)
	}
	if !st2.PendingSince.IsZero() {
		t.Fatalf("删机后 PendingSince 应清零")
	}
}

func TestColdStartSuccessResetsFails(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", true)}
	st := State{PendingSince: base.Add(-5 * time.Minute), ColdStartFails: 1}
	_, st2 := Decide(cfg(), st, o)
	if st2.ColdStartFails != 0 || !st2.PendingSince.IsZero() {
		t.Fatalf("runner 上线应重置冷启状态，got %+v", st2)
	}
}

func TestColdStartFailsExhaustedAlertsInsteadOfCreate(t *testing.T) {
	o := obs()
	o.QueuedRuns = 1
	st := State{ColdStartFails: 2} // 达默认上限
	acts, _ := Decide(cfg(), st, o)
	if hasCreate(acts) {
		t.Fatalf("连续冷启失败达上限不应再自动建机：%v", acts)
	}
	if !hasAlert(acts) {
		t.Fatalf("应告警提示人工介入：%v", acts)
	}
}

func TestIdleTeardownAfterTimeout(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", false)}
	st := State{IdleSince: base.Add(-21 * time.Minute)}
	acts, st2 := Decide(cfg(), st, o)
	if !hasDelete(acts) {
		t.Fatalf("空闲超时应删机：%v", acts)
	}
	if !st2.IdleSince.IsZero() {
		t.Fatalf("删机后 IdleSince 应清零")
	}
}

func TestIdleWithinTimeoutKeepsMachine(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", false)}
	st := State{IdleSince: base.Add(-5 * time.Minute)}
	acts, st2 := Decide(cfg(), st, o)
	if hasDelete(acts) {
		t.Fatalf("空闲未超时不应删机：%v", acts)
	}
	if st2.IdleSince != st.IdleSince {
		t.Fatalf("IdleSince 应保持")
	}
}

func TestIdleFirstSampleSetsIdleSince(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", false)}
	acts, st := Decide(cfg(), State{}, o)
	if hasDelete(acts) {
		t.Fatalf("首个空闲采样不应立即删机：%v", acts)
	}
	if st.IdleSince != base {
		t.Fatalf("IdleSince 应记为本次采样时间，got %v", st.IdleSince)
	}
}

func TestBusyRunnerIsNotIdle(t *testing.T) {
	o := obs()
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", true)}
	st := State{IdleSince: base.Add(-25 * time.Minute)} // 早已超时
	acts, st2 := Decide(cfg(), st, o)
	if hasDelete(acts) {
		t.Fatalf("runner 正忙不应判空闲删机：%v", acts)
	}
	if !st2.IdleSince.IsZero() {
		t.Fatalf("busy 应清零 IdleSince")
	}
}

func TestQueuedResetsIdleSince(t *testing.T) {
	o := obs()
	o.QueuedRuns = 2
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", true)}
	st := State{IdleSince: base.Add(-30 * time.Minute)}
	_, st2 := Decide(cfg(), st, o)
	if !st2.IdleSince.IsZero() {
		t.Fatalf("有队列应清零 IdleSince")
	}
}

func TestStallReplaceAfterTwoSamples(t *testing.T) {
	o := obs()
	o.QueuedRuns = 1
	o.OldestQueuedAge = 16 * time.Minute // 超过 15m 阈值
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", false)} // online 不接活（P139）
	st := State{StallSamples: 1}                                  // 上一轮已采样一次
	acts, st2 := Decide(cfg(), st, o)
	if !hasDelete(acts) {
		t.Fatalf("连续两次停摆采样应换机（删后下轮重建）：%v", acts)
	}
	if st2.StallSamples != 0 {
		t.Fatalf("换机后 StallSamples 应清零")
	}
}

func TestStallFirstSampleOnlyCounts(t *testing.T) {
	o := obs()
	o.QueuedRuns = 1
	o.OldestQueuedAge = 16 * time.Minute
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", false)}
	acts, st := Decide(cfg(), State{}, o)
	if hasDelete(acts) {
		t.Fatalf("单次采样不应立即换机：%v", acts)
	}
	if st.StallSamples != 1 {
		t.Fatalf("StallSamples 应计 1，got %d", st.StallSamples)
	}
}

func TestStallBusyRunnerNotStall(t *testing.T) {
	o := obs()
	o.QueuedRuns = 1
	o.OldestQueuedAge = 40 * time.Minute // 排队很老，但 runner 正忙（串行消化中，nms 单 run ≈23m 属正常）
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", true)}
	st := State{StallSamples: 1}
	acts, st2 := Decide(cfg(), st, o)
	if hasDelete(acts) {
		t.Fatalf("busy runner 不构成停摆：%v", acts)
	}
	if st2.StallSamples != 0 {
		t.Fatalf("busy 应清零 StallSamples")
	}
}

func TestStaleOfflineRegistrationRemoved(t *testing.T) {
	o := obs()
	o.QueuedRuns = 0
	// 机器已删，注册残留 offline；另有一台旧手工 runner 不同前缀（pilot 期共存）
	o.Runners = []Runner{
		{ID: 7, Name: "cirunner-team2-sgp1-01", Status: "offline", Busy: false},
		{ID: 8, Name: "gitrunner-sgp2", Status: "online", Busy: false},
	}
	acts, _ := Decide(cfg(), State{}, o)
	if !hasRemoveReg(acts, "cirunner-team2-sgp1-01") {
		t.Fatalf("应清理本前缀的 offline 残留注册：%v", acts)
	}
	if hasRemoveReg(acts, "gitrunner-sgp2") {
		t.Fatalf("不得动外部 runner（前缀不匹配）：%v", acts)
	}
}

func TestOnlineRunnerWithoutDropletKept(t *testing.T) {
	o := obs()
	o.Runners = []Runner{{ID: 7, Name: "cirunner-team2-sgp1-01", Status: "online", Busy: true}}
	acts, _ := Decide(cfg(), State{}, o)
	if hasRemoveReg(acts, "cirunner-team2-sgp1-01") {
		t.Fatalf("online 注册（可能刚删机心跳未失效）不应立即清：%v", acts)
	}
}

func TestMaxRunnersTwoCreatesSecond(t *testing.T) {
	c := cfg()
	c.MaxRunners = 2
	o := obs()
	o.QueuedRuns = 3
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")}
	o.Runners = []Runner{runner("cirunner-team2-sgp1-01", true)} // 一台在忙
	acts, _ := Decide(c, State{}, o)
	if !hasCreate(acts) {
		t.Fatalf("max=2 且有排队应建第二台：%v", acts)
	}
}

func TestPendingDropletBlocksSecondCreate(t *testing.T) {
	c := cfg()
	c.MaxRunners = 2
	o := obs()
	o.QueuedRuns = 1
	o.Droplets = []Droplet{droplet(1, "cirunner-team2-sgp1-01")} // 冷启中，无 online runner
	acts, _ := Decide(c, State{}, o)
	if hasCreate(acts) {
		t.Fatalf("已有冷启中的机器时不应再建（避免同仓并发冷启）：%v", acts)
	}
}
