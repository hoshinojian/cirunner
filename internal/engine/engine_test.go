package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"github.com/hoshinojian/cirunner/internal/config"
	"github.com/hoshinojian/cirunner/internal/decide"
	"github.com/hoshinojian/cirunner/internal/githubapi"
	"github.com/hoshinojian/cirunner/internal/vpsctlapi"
)

type fakeGH struct {
	queued   githubapi.QueuedInfo
	runners  []githubapi.Runner
	token    string
	removed  []int64
	queuedFn func(repo string) githubapi.QueuedInfo
}

func (f *fakeGH) QueuedRuns(ctx context.Context, repo string) (githubapi.QueuedInfo, error) {
	if f.queuedFn != nil {
		return f.queuedFn(repo), nil
	}
	return f.queued, nil
}
func (f *fakeGH) Runners(ctx context.Context, repo string) ([]githubapi.Runner, error) {
	return f.runners, nil
}
func (f *fakeGH) MintToken(ctx context.Context, repo string) (string, error) {
	return f.token, nil
}
func (f *fakeGH) RemoveRunner(ctx context.Context, repo string, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}

type fakeVPS struct {
	droplets []vpsctlapi.Droplet
	created  []vpsctlapi.CreateOpts
	deleted  []string
}

func (f *fakeVPS) Create(ctx context.Context, o vpsctlapi.CreateOpts) (vpsctlapi.Droplet, error) {
	f.created = append(f.created, o)
	d := vpsctlapi.Droplet{Account: o.Account, ID: fmt.Sprintf("%d", 100+len(f.created)), Name: fmt.Sprintf("%s-%s-%s-%02d", o.NamePrefix, o.Account, o.Region, o.StartIndex), IPv4Public: "1.2.3.4"}
	f.droplets = append(f.droplets, d)
	return d, nil
}
func (f *fakeVPS) ListByTag(ctx context.Context, tag, account string) ([]vpsctlapi.Droplet, error) {
	if tag == "cirunner" {
		return f.droplets, nil
	}
	var out []vpsctlapi.Droplet
	for _, d := range f.droplets {
		for _, t := range d.Tags {
			if t == tag {
				out = append(out, d)
				break
			}
		}
	}
	return out, nil
}
func (f *fakeVPS) Delete(ctx context.Context, account string, id string) error {
	f.deleted = append(f.deleted, id)
	var out []vpsctlapi.Droplet
	for _, d := range f.droplets {
		if d.ID != id {
			out = append(out, d)
		}
	}
	f.droplets = out
	return nil
}

func testRepo() config.Repo {
	return config.Repo{
		Repo: "hoshinojian/nms", Account: "team2-ci", Region: "sgp1",
		Size: "c2-4vcpu-8gb", Image: "ubuntu-24-04-x64", NamePrefix: "cirunner",
		Labels: []string{"ci"}, MaxRunners: 1,
	}
}

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		VPSctlBin: "/x/vpsctl", GhBin: "gh", IdleTimeoutMin: 20, MaxDroplets: 2,
		StateDir: t.TempDir(), Repos: []config.Repo{testRepo()},
	}
}

func newEngine(cfg *config.Config, gh *fakeGH, vps *fakeVPS) *Engine {
	lg := log.New(io.Discard, "", 0)
	e := New(cfg, gh, vps, []string{"ssh-ed25519 AAAAkey"}, lg)
	_ = e.LoadStates(cfg.StateDir + "/states.json")
	return e
}

func TestNextNameIncrements(t *testing.T) {
	r := testRepo()
	existing := []decide.Droplet{
		{Name: "cirunner-team2-ci-sgp1-01"},
		{Name: "cirunner-team2-ci-sgp1-03"},
		{Name: "cirunner-team2-ci-sgp2-07"}, // 同前缀不同区域，不占本区序号
		{Name: "gitrunner-team2-sgp1-02"},   // 外部前缀
	}
	got, err := NextName(r, existing)
	if err != nil || got != "cirunner-team2-ci-sgp1-04" {
		t.Fatalf("应取 04，got %s err %v", got, err)
	}
	fresh, _ := NextName(r, nil)
	if fresh != "cirunner-team2-ci-sgp1-01" {
		t.Fatalf("空场应取 01，got %s", fresh)
	}
}

func TestMatchesNaming(t *testing.T) {
	r := testRepo()
	cases := map[string]bool{
		"cirunner-team2-ci-sgp1-01":                  true,
		"cirunner-team2-ci-sgp1-99":                  true,
		"gitrunner-sgp2":                             false, // pilot 期旧手工机，不碰
		"cirunner-team2-ci-sgp2-01":                  false, // 不同区域
		"cirunner-team2-ci-sgp1-xx":                  false,
		"evil-prefix" + "-cirunner-team2-ci-sgp1-01": false,
	}
	for name, want := range cases {
		if got := MatchesNaming(r, name); got != want {
			t.Errorf("MatchesNaming(%q)=%v want %v", name, got, want)
		}
	}
}

func TestReconcileCreatePathEndToEnd(t *testing.T) {
	cfg := testCfg(t)
	gh := &fakeGH{token: "AGB2ZQKHL4TGHTRSEXAMPLE", queued: githubapi.QueuedInfo{Count: 1, OldestAge: 2 * time.Minute, ObservedAt: time.Now()}}
	vps := &fakeVPS{}
	e := newEngine(cfg, gh, vps)
	budget := 2
	acts, err := e.Reconcile(context.Background(), testRepo(), false, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(vps.created) != 1 {
		t.Fatalf("应建 1 台，got %d（acts=%+v）", len(vps.created), acts)
	}
	o := vps.created[0]
	if o.StartIndex != 1 || o.Account != "team2-ci" {
		t.Fatalf("建机参数不对: %+v", o)
	}
	for _, tag := range []string{"cirunner", "repo:nms"} {
		found := false
		for _, tg := range o.Tags {
			if tg == tag {
				found = true
			}
		}
		if !found {
			t.Errorf("建机 tags 缺 %q: %v", tag, o.Tags)
		}
	}
	if budget != 1 {
		t.Fatalf("建机应扣配额，budget=%d", budget)
	}
	// 建机成功后 cloud-init 文件（含 token）应被删除
	if _, err := os.ReadFile(cfg.StateDir + "/cloudinit/cirunner-team2-ci-sgp1-01.yaml"); err == nil {
		t.Fatal("建机成功后 cloud-init 文件应删除（含 1h token）")
	}
}

func TestReconcileBudgetBlocksCreate(t *testing.T) {
	cfg := testCfg(t)
	gh := &fakeGH{token: "AGB2ZQKHL4TGHTRSEXAMPLE", queued: githubapi.QueuedInfo{Count: 1, OldestAge: time.Minute, ObservedAt: time.Now()}}
	vps := &fakeVPS{}
	e := newEngine(cfg, gh, vps)
	budget := 0
	if _, err := e.Reconcile(context.Background(), testRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(vps.created) != 0 {
		t.Fatal("配额为 0 不应建机")
	}
}

func TestReconcileIdleTeardownRemovesDropletThenRegistration(t *testing.T) {
	cfg := testCfg(t)
	vps := &fakeVPS{droplets: []vpsctlapi.Droplet{
		{Account: "team2-ci", ID: "5", Name: "cirunner-team2-ci-sgp1-01", Tags: []string{"cirunner", "repo:nms"}},
	}}
	gh := &fakeGH{queued: githubapi.QueuedInfo{ObservedAt: time.Now()}}
	e := newEngine(cfg, gh, vps)
	budget := 2

	// 第一轮：空闲开始计时
	if _, err := e.Reconcile(context.Background(), testRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(vps.deleted) != 0 {
		t.Fatal("首采样不应删机")
	}
	// 快进状态：把 IdleSince 拨回 21 分钟前
	st := e.States["hoshinojian/nms"]
	st.IdleSince = time.Now().Add(-21 * time.Minute)
	e.States["hoshinojian/nms"] = st
	// runner 此时应表现为 offline（机器还在，正常 online idle；保持 online idle 即可触发删机）
	gh.runners = []githubapi.Runner{{ID: 42, Name: "cirunner-team2-ci-sgp1-01", Status: "online", Busy: false}}
	if _, err := e.Reconcile(context.Background(), testRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(vps.deleted) != 1 || vps.deleted[0] != "5" {
		t.Fatalf("空闲超时应删机 id=5，got %v", vps.deleted)
	}
	// 第三轮：机器已删，runner 转 offline 残留 → 清注册
	gh.runners = []githubapi.Runner{{ID: 42, Name: "cirunner-team2-ci-sgp1-01", Status: "offline", Busy: false}}
	if _, err := e.Reconcile(context.Background(), testRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(gh.removed) != 1 || gh.removed[0] != 42 {
		t.Fatalf("应清残留注册 id=42，got %v", gh.removed)
	}
}

func TestTeardownRefusesForeignRunners(t *testing.T) {
	cfg := testCfg(t)
	vps := &fakeVPS{droplets: []vpsctlapi.Droplet{
		{Account: "team2-ci", ID: "5", Name: "cirunner-team2-ci-sgp1-01", Tags: []string{"cirunner", "repo:nms"}},
	}}
	gh := &fakeGH{runners: []githubapi.Runner{
		{ID: 42, Name: "cirunner-team2-ci-sgp1-01", Status: "online"},
		{ID: 43, Name: "gitrunner-sgp2", Status: "online"}, // pilot 期旧机，不碰
	}}
	e := newEngine(cfg, gh, vps)
	if err := e.Teardown(context.Background(), testRepo()); err != nil {
		t.Fatal(err)
	}
	if len(vps.deleted) != 1 || vps.deleted[0] != "5" {
		t.Fatal("down 应删本仓 droplet")
	}
	if len(gh.removed) != 1 || gh.removed[0] != 42 {
		t.Fatalf("down 只应清本命名空间注册，got %v", gh.removed)
	}
}

func TestStatesPersist(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/states.json"
	e := &Engine{States: map[string]decide.State{"a/b": {StallSamples: 3}}}
	e.statePath = path
	if err := e.SaveStates(); err != nil {
		t.Fatal(err)
	}
	e2 := &Engine{States: map[string]decide.State{}}
	e2.statePath = path
	if err := e2.LoadStates(path); err != nil {
		t.Fatal(err)
	}
	if e2.States["a/b"].StallSamples != 3 {
		t.Fatal("状态持久化往返不一致")
	}
}
