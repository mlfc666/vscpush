// vscpush —— 一条命令完成 VS Code Server 的「本地多线程下载 + 推送安装」。
//
// 设计取舍：
//   - 下载：不依赖 aria2c/curl，内置 HTTP Range 并发下载器，进度、重试、限速都由自己控制。
//   - 上传：不依赖 scp/rsync，内置 SSH/SFTP 客户端（支持 ssh-agent、私钥、密码）。
//   - 校验：本地算 sha256，上传后在远端用 sha256sum 复算并比对，避免半包。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	toolName    = "vscpush"
	toolVersion = "0.3.1"
)

type config struct {
	commit     string
	host       string
	user       string
	port       int
	arch       string
	conns      int
	keyFile    string
	password   string
	remoteDir  string
	remoteName string
	localDir   string
	install    bool
	installDir string
	force      bool
	dryRun     bool
	quiet      bool
	noVerify   bool
	noUpload   bool
	url        string
	engine     string
	strictHost bool
	timeout    time.Duration
}

func main() {
	c, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		fmt.Fprintln(os.Stderr, "用 -h 查看用法")
		os.Exit(2)
	}
	if c == nil { // -h / -version
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := run(ctx, c); err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Fprintln(os.Stderr, "\n已取消")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "\n失败:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	var showVersion, showHelp bool
	fs := flag.CommandLine
	fs.Usage = usage

	fs.StringVar(&c.commit, "commit", "", "VS Code 的 commit id（必填，报错日志里的那串 40 位十六进制）")
	fs.StringVar(&c.host, "host", "", "目标服务器地址，IP 或域名（必填）")
	fs.StringVar(&c.user, "user", "", "目标服务器 SSH 登录用户（必填）")
	fs.IntVar(&c.port, "port", 22, "SSH 端口")
	fs.StringVar(&c.arch, "arch", "auto", "服务端架构：auto（登录后自动探测）/ x64 / arm64 / armhf")
	fs.IntVar(&c.conns, "conns", 16, "下载并发连接数；CDN 按连接限速时，并发能成倍提速")
	fs.StringVar(&c.keyFile, "key", "", "SSH 私钥路径（默认依次尝试 ~/.ssh/id_ed25519、id_ecdsa、id_rsa，以及 ssh-agent）")
	fs.StringVar(&c.password, "password", "", "SSH 密码；更推荐用环境变量 VSCPUSH_PASSWORD，避免留在 shell 历史里")
	fs.StringVar(&c.remoteDir, "remote-dir", "", "远端落地目录（默认登录用户的主目录）")
	fs.StringVar(&c.remoteName, "remote-name", "", "远端文件名（默认 vscode-server-linux-<arch>.tar.gz）")
	fs.StringVar(&c.localDir, "local-dir", os.TempDir(), "本地临时下载目录")
	fs.BoolVar(&c.install, "install", false, "上传后就地解包安装到 ~/.vscode-server/bin/<commit>")
	fs.StringVar(&c.installDir, "install-dir", "", "解包目标目录（默认 ~/.vscode-server/bin/<commit>）")
	fs.BoolVar(&c.force, "force", false, "即使该 commit 的 server 正在运行也强制覆盖安装（会打断在线会话）")
	fs.BoolVar(&c.dryRun, "dry-run", false, "只探测并打印执行计划，不下载、不上传")
	fs.BoolVar(&c.quiet, "quiet", false, "不打印进度条")
	fs.BoolVar(&c.noVerify, "no-verify", false, "跳过上传后的远端 sha256 复算校验")
	fs.BoolVar(&c.noUpload, "no-upload", false, "只下载到本地，不上传（配合 -arch 可完全不连远端）")
	fs.StringVar(&c.url, "url", "", "覆盖下载地址，用于自建镜像或离线缓存服务器")
	fs.StringVar(&c.engine, "engine", "auto", "下载引擎：auto（有 aria2c 就用）/ aria2c / builtin")
	fs.BoolVar(&c.strictHost, "strict-host-key", false, "严格主机密钥校验：不在 known_hosts 里就直接拒绝")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Minute, "整体超时时间")
	fs.BoolVar(&showVersion, "version", false, "打印版本号后退出")
	fs.BoolVar(&showHelp, "help", false, "打印帮助后退出")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil // usage 已由 flag 打印
		}
		return nil, err
	}
	if showVersion {
		fmt.Printf("%s v%s\n", toolName, toolVersion)
		return nil, nil
	}
	if showHelp {
		usage()
		return nil, nil
	}

	if c.password == "" {
		c.password = os.Getenv("VSCPUSH_PASSWORD")
	}
	var missing []string
	if strings.TrimSpace(c.commit) == "" {
		missing = append(missing, "-commit")
	}
	if strings.TrimSpace(c.host) == "" {
		missing = append(missing, "-host")
	}
	if strings.TrimSpace(c.user) == "" {
		missing = append(missing, "-user")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少必填参数 %s", strings.Join(missing, "、"))
	}
	if c.conns < 1 {
		c.conns = 1
	}
	if c.conns > 32 {
		c.conns = 32
	}
	switch c.engine {
	case EngineAuto, EngineAria2, EngineBuiltin:
	default:
		return nil, fmt.Errorf("-engine 只能是 auto / aria2c / builtin，收到 %q", c.engine)
	}
	if c.port < 1 || c.port > 65535 {
		return nil, fmt.Errorf("端口不合法: %d", c.port)
	}
	if c.localDir == "" {
		c.localDir = os.TempDir()
	}
	quietProgress = c.quiet
	return c, nil
}

func usage() {
	out := os.Stderr
	fmt.Fprintf(out, `%s v%s —— 一条命令完成 VS Code Server 的高速下载 + 推送（也可顺带安装）

用法：
  %s -commit <commit-id> -host <ip或域名> -user <用户名> [选项]

必填：
  -commit    VS Code 报错日志里的 40 位 commit id
  -host      目标服务器地址
  -user      目标服务器 SSH 用户

示例：
  # 下载 + 上传到主目录（arm64 自动识别）
  %s -commit 69047191fc427f2f92828279aae1d90d8eeda7ab -host 10.24.10.224 -user liaobin

  # 顺手装好，装完打印 node 版本
  %s -commit 69047191fc427f2f92828279aae1d90d8eeda7ab -host 10.24.10.224 -user liaobin --install

  # 只看看要做什么
  %s -commit 69047191fc427f2f92828279aae1d90d8eeda7ab -host 10.24.10.224 -user liaobin --dry-run

选项：
`, toolName, toolVersion, toolName, toolName, toolName, toolName)
	flag.PrintDefaults()
}
