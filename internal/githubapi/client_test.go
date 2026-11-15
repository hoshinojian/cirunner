package githubapi

import (
	"context"
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
