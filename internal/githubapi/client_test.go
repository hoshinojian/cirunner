package githubapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fake(out string, err error) *Client {
	c := NewClient("gh")
	c.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(out), err
	}
	return c
}

// fakeRouter 按 URL 片段分流输出（模拟 runs 列表与逐 run jobs 两类端点）。
func fakeRouter(routes map[string]string) *Client {
	c := NewClient("gh")
	c.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, a := range args {
			for frag, out := range routes {
				if strings.Contains(a, frag) {
					return []byte(out), nil
				}
			}
		}
		return nil, fmt.Errorf("fake: 未路由的调用 %v", args)
	}
	return c
}

func TestQueuedRunsParses(t *testing.T) {
	oldest := time.Now().UTC().Add(-16 * time.Minute).Format(time.RFC3339)
	c := fake(`{"n":2,"oldest":"`+oldest+`"}`, nil)
	q, err := c.QueuedRuns(context.Background(), "hoshinojian/nms")
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 2 {
		t.Fatalf("count 应为 2，got %d", q.Count)
	}
	if q.OldestAge < 15*time.Minute || q.OldestAge > 17*time.Minute {
		t.Fatalf("oldest age 应约 16m，got %v", q.OldestAge)
	}
}

func TestQueuedEmpty(t *testing.T) {
	c := fake(`{"n":0,"oldest":null}`, nil)
	q, err := c.QueuedRuns(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 0 || q.OldestAge != 0 {
		t.Fatalf("空队列应零值，got %+v", q)
	}
}

func TestRunnersParses(t *testing.T) {
	c := fake(`[{"id":1,"name":"cirunner-team2-ci-sgp1-01","status":"online","busy":true},{"id":2,"name":"gitrunner-sgp2","status":"online","busy":false}]`, nil)
	rs, err := c.Runners(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Name != "cirunner-team2-ci-sgp1-01" || !rs[0].Busy {
		t.Fatalf("解析不对: %+v", rs)
	}
}

func TestMintTokenTrims(t *testing.T) {
	c := fake("  ABCTOKEN123456789  \n", nil)
	tok, err := c.MintToken(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ABCTOKEN123456789" {
		t.Fatalf("token 应去空白，got %q", tok)
	}
}

func TestMintTokenTooShort(t *testing.T) {
	c := fake("short", nil)
	if _, err := c.MintToken(context.Background(), "a/b"); err == nil {
		t.Fatal("异常短 token 应报错")
	}
}

// 热补丁缺陷 #2：run 级 status 不可靠（P141）——run 一旦有 job 开跑即变
// in_progress，其余排队 job 对 status=queued 过滤不可见。排队信号必须补拉
// in_progress run 的 jobs 才能数到这些 job。
func TestInProgressQueuedJobsParses(t *testing.T) {
	now := time.Now().UTC()
	older := now.Add(-9 * time.Minute).Format(time.RFC3339)
	newer := now.Add(-2 * time.Minute).Format(time.RFC3339)
	c := fakeRouter(map[string]string{
		"actions/runs?status=in_progress": `[44237158062, 44237158063]`,
		"runs/44237158062/jobs":           fmt.Sprintf(`{"n":2,"oldest":"%s"}`, older),
		"runs/44237158063/jobs":           fmt.Sprintf(`{"n":1,"oldest":"%s"}`, newer),
	})
	q, err := c.InProgressQueuedJobs(context.Background(), "hoshinojian/nms")
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 3 {
		t.Fatalf("in_progress run 内 queued job 应合计 3，got %d", q.Count)
	}
	if q.OldestAge < 8*time.Minute || q.OldestAge > 10*time.Minute {
		t.Fatalf("最老 queued job 年龄应约 9m，got %v", q.OldestAge)
	}
}

func TestInProgressQueuedJobsNoRuns(t *testing.T) {
	c := fakeRouter(map[string]string{
		"actions/runs?status=in_progress": `[]`,
	})
	q, err := c.InProgressQueuedJobs(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 0 || q.OldestAge != 0 {
		t.Fatalf("无 in_progress run 应零值，got %+v", q)
	}
}

func TestInProgressQueuedJobsRunWithoutQueuedJob(t *testing.T) {
	c := fakeRouter(map[string]string{
		"actions/runs?status=in_progress": `[1]`,
		"runs/1/jobs":                     `{"n":0,"oldest":null}`,
	})
	q, err := c.InProgressQueuedJobs(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 0 {
		t.Fatalf("run 全部 job 在跑不应计入排队，got %d", q.Count)
	}
}

func TestMergeQueuedTakesSumAndOldest(t *testing.T) {
	now := time.Now().UTC()
	// 两路观察几乎同时：queued run 1 个最老 20m；in_progress queued job 2 个最老 4m
	a := QueuedInfo{Count: 1, OldestAge: 20 * time.Minute, ObservedAt: now.Add(-time.Second)}
	b := QueuedInfo{Count: 2, OldestAge: 4 * time.Minute, ObservedAt: now}
	m := MergeQueued(a, b)
	if m.Count != 3 {
		t.Fatalf("合计应为 3，got %d", m.Count)
	}
	// 最老应取 queued run 那路（20m 量级，而非 4m）
	if m.OldestAge < 19*time.Minute || m.OldestAge > 21*time.Minute {
		t.Fatalf("最老排队年龄应约 20m，got %v", m.OldestAge)
	}
	if !m.ObservedAt.Equal(now) {
		t.Fatalf("基准时刻应取较新者，got %v", m.ObservedAt)
	}
}

func TestMergeQueuedEitherSideEmpty(t *testing.T) {
	now := time.Now().UTC()
	empty := QueuedInfo{ObservedAt: now}
	b := QueuedInfo{Count: 2, OldestAge: 5 * time.Minute, ObservedAt: now}
	if m := MergeQueued(empty, b); m.Count != 2 || m.OldestAge != b.OldestAge {
		t.Fatalf("左空应透传右路，got %+v", m)
	}
	a := QueuedInfo{Count: 1, OldestAge: 7 * time.Minute, ObservedAt: now}
	if m := MergeQueued(a, empty); m.Count != 1 || m.OldestAge != a.OldestAge {
		t.Fatalf("右空应透传左路，got %+v", m)
	}
	if m := MergeQueued(empty, empty); m.Count != 0 || m.OldestAge != 0 {
		t.Fatalf("双空应零值，got %+v", m)
	}
}
