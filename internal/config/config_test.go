package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cirunner.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `{
  "repos": [
    {"repo": "hoshinojian/nms", "account": "team2", "region": "sgp1",
     "size": "c2-4vcpu-8gb", "image": "ubuntu-24-04-x64"}
  ]
}`

// valid 是最小合法配置（含必填的 vpsctl_bin）。
const valid = `{
  "vpsctl_bin": "/home/x/vpsctl/bin/vpsctl",
  "repos": [
    {"repo": "hoshinojian/nms", "account": "team2", "region": "sgp1",
     "size": "c2-4vcpu-8gb", "image": "ubuntu-24-04-x64"}
  ]
}`

func TestDefaultsApplied(t *testing.T) {
	c, err := Load(writeTemp(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.GhBin != "gh" || c.PollIntervalSec != 60 || c.IdleTimeoutMin != 20 || c.MaxDroplets != 2 {
		t.Fatalf("全局默认不对: %+v", c)
	}
	r := c.Repos[0]
	if r.NamePrefix != "cirunner" || r.MaxRunners != 1 {
		t.Fatalf("每仓默认不对: %+v", r)
	}
	if len(r.Labels) != 1 || r.Labels[0] != "ci" {
		t.Fatalf("labels 默认应为 [ci]: %v", r.Labels)
	}
}

func TestDerivedFields(t *testing.T) {
	c, err := Load(writeTemp(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Repos[0]
	if r.Slug() != "nms" {
		t.Fatalf("Slug 应为 nms，got %s", r.Slug())
	}
	if r.RepoTag() != "repo:nms" {
		t.Fatalf("RepoTag 应为 repo:nms，got %s", r.RepoTag())
	}
	if r.ServiceRepoPart() != "hoshinojian-nms" {
		t.Fatalf("systemd unit 仓名应为连字符形式，got %s", r.ServiceRepoPart())
	}
	if r.URL() != "https://github.com/hoshinojian/nms" {
		t.Fatalf("URL 不对: %s", r.URL())
	}
}

func TestHomeExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()
	c := &Config{VPSctlBin: "~/x/vpsctl", Repos: []Repo{{Repo: "a/b", Account: "t", Region: "r", Size: "s", Image: "i"}}}
	if err := c.expandHome(); err != nil {
		t.Fatal(err)
	}
	if c.VPSctlBin != filepath.Join(home, "x", "vpsctl") {
		t.Fatalf("~ 展开不对: %s", c.VPSctlBin)
	}
}

func TestMissingAccountRejected(t *testing.T) {
	// 2026-11-13 教训：vpsctl create 漏 -only 全账号各建一台 —— account 必填
	bad := `{"vpsctl_bin":"/x/vpsctl","repos":[{"repo":"hoshinojian/nms","region":"sgp1","size":"c2-4vcpu-8gb","image":"ubuntu-24-04-x64"}]}`
	if _, err := Load(writeTemp(t, bad)); err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("缺 account 应报错，got %v", err)
	}
}

func TestBadRepoFormatRejected(t *testing.T) {
	for _, bad := range []string{"nms", "a/b/c", "a/", "/b", "a/ b"} {
		j := `{"vpsctl_bin":"/x","repos":[{"repo":"` + bad + `","account":"t","region":"r","size":"s","image":"i"}]}`
		if _, err := Load(writeTemp(t, j)); err == nil {
			t.Fatalf("repo=%q 应报错", bad)
		}
	}
}

func TestDuplicateRepoRejected(t *testing.T) {
	dup := `{"vpsctl_bin":"/x","repos":[
		{"repo":"hoshinojian/nms","account":"t","region":"r","size":"s","image":"i"},
		{"repo":"hoshinojian/nms","account":"t","region":"r","size":"s","image":"i"}]}`
	if _, err := Load(writeTemp(t, dup)); err == nil {
		t.Fatal("重复 repo 应报错")
	}
}

func TestVPSctlBinRequired(t *testing.T) {
	if _, err := Load(writeTemp(t, minimal)); err == nil {
		t.Fatal("缺 vpsctl_bin 应报错（不猜 PATH，显式锚定二进制）")
	}
}

func TestMissingFileWithEnvFallback(t *testing.T) {
	t.Setenv("CIRUNNER_CONFIG", "/nonexistent/x.json")
	if _, err := Load(""); err == nil {
		t.Fatal("不存在的配置文件应报错")
	}
}

func TestLabelValidation(t *testing.T) {
	bad := `{"vpsctl_bin":"/x","repos":[{"repo":"a/b","account":"t","region":"r","size":"s","image":"i","labels":["bad label"]}]}`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("label 含空格应报错（会写进 shell 命令）")
	}
}
