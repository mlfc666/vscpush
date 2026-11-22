package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseContentRangeTotal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes 0-0/236720088", 236720088, true},
		{"bytes 100-199/1000", 1000, true},
		{"bytes */1234", 1234, true},
		{"garbage", 0, false},
		{"bytes 0-0/*", 0, false},
	}
	for _, c := range cases {
		got, err := parseContentRangeTotal(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("parseContentRangeTotal(%q) = %d, %v; want %d, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote("it's here")
	want := `'it'\''s here'`
	if got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
	if got := shellQuote("/home/u/.vscode-server/bin/abc"); got != "'/home/u/.vscode-server/bin/abc'" {
		t.Errorf("plain path quoting = %q", got)
	}
}

func TestAbsPath(t *testing.T) {
	r := &remoteConn{home: "/home/liaobin"}
	cases := map[string]string{
		"":                        "/home/liaobin",
		"~":                       "/home/liaobin",
		"~/x/y.tar.gz":            "/home/liaobin/x/y.tar.gz",
		"x/y.tar.gz":              "/home/liaobin/x/y.tar.gz",
		"/tmp/abs.tar.gz":         "/tmp/abs.tar.gz",
		"vscode-server-linux.tar": "/home/liaobin/vscode-server-linux.tar",
	}
	for in, want := range cases {
		if got := r.absPath(in); got != want {
			t.Errorf("absPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckArch(t *testing.T) {
	for _, a := range []string{"x64", "arm64", "armhf"} {
		if err := checkArch(a); err != nil {
			t.Errorf("checkArch(%q) = %v", a, err)
		}
	}
	for _, a := range []string{"", "amd64", "aarch64", "alpine-x64"} {
		if err := checkArch(a); err == nil {
			t.Errorf("checkArch(%q) 应该报错", a)
		}
	}
}

func TestHumanBytesAndDur(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		512:       "512 B",
		1536:      "1.5 KiB",
		236720088: "225.8 MiB",
		5 << 30:   "5.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	if got := fmtDur(4230 * 1e9); got != "1h10m" {
		t.Errorf("fmtDur = %q", got)
	}
}

func TestSplitSpans(t *testing.T) {
	cases := []struct{ size, piece int64 }{
		{0, 1 << 20},
		{1, 1 << 20},
		{1 << 20, 1 << 20},
		{(1 << 20) + 1, 1 << 20},
		{236720088, 1 << 20},
		{100, 7},
	}
	for _, c := range cases {
		sp := splitSpans(c.size, c.piece)
		if c.size == 0 {
			if len(sp) != 0 {
				t.Fatalf("size=0 应该没有片段，得到 %d", len(sp))
			}
			continue
		}
		if sp[0].start != 0 || sp[len(sp)-1].end != c.size-1 {
			t.Fatalf("size=%d piece=%d 覆盖不对: %v", c.size, c.piece, sp)
		}
		for i, s := range sp {
			if s.end < s.start {
				t.Fatalf("片段 %d 区间非法: %v", i, s)
			}
			if i > 0 && sp[i-1].end+1 != s.start {
				t.Fatalf("片段 %d 不连续: %v -> %v", i, sp[i-1], s)
			}
		}
	}
}

func TestPickEngine(t *testing.T) {
	if got := pickEngine(EngineBuiltin); got != EngineBuiltin {
		t.Errorf("builtin 请求得到 %q", got)
	}
	if got := pickEngine(EngineAria2); got != EngineAria2 {
		t.Errorf("aria2c 请求得到 %q", got)
	}
	if _, has := aria2Path(); has {
		if got := pickEngine(EngineAuto); got != EngineAria2 {
			t.Errorf("有 aria2c 时 auto 应选 aria2c，得到 %q", got)
		}
	} else if got := pickEngine(EngineAuto); got != EngineBuiltin {
		t.Errorf("无 aria2c 时 auto 应选 builtin，得到 %q", got)
	}
}

func TestHostMatches(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"10.21.243.251", "10.21.243.251", true},
		{"10.21.243.251,10.0.0.1", "10.0.0.1", true},
		{"10.21.243.251,10.0.0.1", "10.21.243.251", true},
		{"10.21.243.*", "10.21.243.251", true},
		{"[10.21.243.251]:2222", "[10.21.243.251]:2222", true},
		{"10.21.243.251", "10.21.243.252", false},
		{"|1|abc=|def=", "10.21.243.251", false}, // 哈希记录不参与排序
	}
	for _, c := range cases {
		if got := hostMatches(c.pattern, c.host); got != c.want {
			t.Errorf("hostMatches(%q,%q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestPreferredHostKeyAlgorithms(t *testing.T) {
	// 用一个临时 HOME 构造 known_hosts，验证 ed25519 被提到最前
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, ".ssh", "known_hosts")
	content := "10.21.243.251 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK7lh1IM2P90LM+78liFS48UEuTJrikqzOOPJovV2KpJ\n"
	if err := os.WriteFile(kh, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	got := preferredHostKeyAlgorithms(&config{host: "10.21.243.251", port: 22})
	if len(got) == 0 || got[0] != "ssh-ed25519" {
		t.Fatalf("应优先 ssh-ed25519，得到 %v", got)
	}
	// 未知主机时应返回 nil，交回 Go 默认顺序
	if got := preferredHostKeyAlgorithms(&config{host: "192.0.2.99", port: 22}); got != nil {
		t.Errorf("未知主机应返回 nil，得到 %v", got)
	}
}
