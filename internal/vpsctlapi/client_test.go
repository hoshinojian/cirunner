package vpsctlapi

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fakeClient(out string, runErr error) *Client {
	c := NewClient("/usr/bin/true", "")
	c.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(out), runErr
	}
	return c
}

func TestCreateArgs(t *testing.T) {
	c := NewClient("/bin/vpsctl", "/tmp/accounts.json")
	var got []string
	c.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		got = args
		return []byte(`{"batch":"b","created":[{"account":"team2-ci","id":"1","name":"cirunner-team2-ci-sgp1-01","ipv4_public":"1.2.3.4"}],"errors":[]}`), nil
	}
	_, err := c.Create(context.Background(), CreateOpts{
		Account: "team2-ci", Region: "sgp1", Size: "c2-4vcpu-8gb", Image: "ubuntu-24-04-x64",
		NamePrefix: "cirunner", StartIndex: 7, Tags: []string{"cirunner", "repo:nms"},
		UserDataFile: "/tmp/ci.yaml", Wait: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"create", "-accounts /tmp/accounts.json", "-only team2-ci",
		"-region sgp1", "-size c2-4vcpu-8gb", "-image ubuntu-24-04-x64",
		"-name-prefix cirunner", "-start-index 7", "-count 1",
		"-tags cirunner,repo:nms", "-user-data /tmp/ci.yaml", "-wait 5m0s",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("create 参数缺 %q: %s", want, joined)
		}
	}
}

func TestCreateNoIPReportsDropletForManualCleanup(t *testing.T) {
	c := fakeClient(`{"batch":"b","created":[{"account":"team2-ci","id":"9","name":"cirunner-x"}],"errors":[]}`, nil)
	_, err := c.Create(context.Background(), CreateOpts{Account: "a", UserDataFile: "/f", Tags: []string{"t"}})
	if err == nil || !strings.Contains(err.Error(), "id=9") {
		t.Fatalf("无公网 IP 应报错并带 id 供手动处置，got %v", err)
	}
}

func TestCreatePartialFailure(t *testing.T) {
	c := fakeClient(`{"batch":"b","created":[],"errors":[{"account":"a","name":"n","error":"boom"}]}`, nil)
	if _, err := c.Create(context.Background(), CreateOpts{Account: "a", UserDataFile: "/f", Tags: []string{"t"}}); err == nil {
		t.Fatal("errors 非空应报错")
	}
}

func TestListParsesDroplets(t *testing.T) {
	c := fakeClient(`{"droplets":[
		{"account":"team2-ci","id":"11","name":"cirunner-team2-ci-sgp1-01","status":"active","ipv4_public":"1.1.1.1","tags":["cirunner","repo:nms"],"created_at":"2026-11-15T10:00:00Z"},
		{"account":"team3","id":"12","name":"other","tags":[]}
	]}`, nil)
	ds, err := c.ListByTag(context.Background(), "cirunner", "team2-ci")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Name != "cirunner-team2-ci-sgp1-01" || ds[0].IPv4Public != "1.1.1.1" {
		t.Fatalf("解析不对: %+v", ds)
	}
	if ds[0].CreatedAt.IsZero() {
		t.Fatal("created_at 应解析")
	}
}

func TestListAccountErrorFails(t *testing.T) {
	c := fakeClient(`{"droplets":[],"errors":[{"account":"x","error":"bad token"}]}`, nil)
	if _, err := c.ListByTag(context.Background(), "cirunner", "team2-ci"); err == nil {
		t.Fatal("账号级失败应报错")
	}
}

func TestDeleteChecksOpResult(t *testing.T) {
	ok := fakeClient(`[{"account":"team2-ci","id":"5","ok":true}]`, nil)
	if err := ok.Delete(context.Background(), "team2-ci", "5"); err != nil {
		t.Fatal(err)
	}
	bad := fakeClient(`[{"account":"team2-ci","id":"5","ok":false,"error":"denied"}]`, nil)
	if err := bad.Delete(context.Background(), "team2-ci", "5"); err == nil {
		t.Fatal("ok=false 应报错")
	}
}
