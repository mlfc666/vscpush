package main

import "testing"

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
