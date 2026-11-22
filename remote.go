package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// remoteConn 封装一条 SSH 连接 + 一个 SFTP 会话，外加远端家目录等上下文。
type remoteConn struct {
	cfg       *config
	ssh       *ssh.Client
	sftp      *sftp.Client
	home      string
	unameArch string
}

func dialRemote(c *config) (*remoteConn, error) {
	methods, err := authMethods(c)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(c)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:              c.user,
		Auth:              methods,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: preferredHostKeyAlgorithms(c),
		Timeout:           20 * time.Second,
	}
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	cl, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接 %s 失败: %w", addr, err)
	}
	sf, err := sftp.NewClient(cl, sftp.MaxPacket(1<<15), sftp.UseConcurrentWrites(true))
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("建立 SFTP 会话失败: %w", err)
	}
	home, err := sf.Getwd()
	if err != nil {
		sf.Close()
		cl.Close()
		return nil, fmt.Errorf("获取远端家目录失败: %w", err)
	}
	return &remoteConn{cfg: c, ssh: cl, sftp: sf, home: home}, nil
}

func (r *remoteConn) Close() {
	if r.sftp != nil {
		_ = r.sftp.Close()
	}
	if r.ssh != nil {
		_ = r.ssh.Close()
	}
}

func (r *remoteConn) exec(cmd string) (string, error) {
	sess, err := r.ssh.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("远端命令失败: %v: %s", err, msg)
	}
	return stdout.String(), nil
}

// detectArch 用 uname -m 判断架构，映射到 VS Code 服务端的平台名。
func (r *remoteConn) detectArch() (string, error) {
	out, err := r.exec("uname -m")
	if err != nil {
		return "", err
	}
	r.unameArch = strings.TrimSpace(out)
	switch r.unameArch {
	case "x86_64", "amd64":
		return "x64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "armv7l", "armv6l", "armv8l", "armhf":
		return "armhf", nil
	}
	return "", fmt.Errorf("无法识别的架构 %q，请用 -arch 显式指定（x64 / arm64 / armhf）", r.unameArch)
}

// absPath 把 -remote-dir / -install-dir 里的相对路径和 ~ 展开成远端绝对路径。
// SFTP 不做 shell 展开，所以 ~ 必须自己处理。
func (r *remoteConn) absPath(p string) string {
	if p == "" || p == "~" {
		return r.home
	}
	if strings.HasPrefix(p, "~/") {
		return path.Join(r.home, p[2:])
	}
	if path.IsAbs(p) {
		return p
	}
	return path.Join(r.home, p)
}

func (r *remoteConn) upload(localPath, remotePath string, pr *progress) error {
	if dir := path.Dir(remotePath); dir != "." && dir != "/" {
		if err := r.sftp.MkdirAll(dir); err != nil {
			return fmt.Errorf("创建远端目录 %s 失败: %w", dir, err)
		}
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := r.sftp.Create(remotePath)
	if err != nil {
		return fmt.Errorf("创建远端文件 %s 失败: %w", remotePath, err)
	}
	_, copyErr := io.CopyBuffer(dst, &countingReader{r: src, p: pr}, make([]byte, 128<<10))
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func (r *remoteConn) remoteSHA256(remotePath string) (string, error) {
	out, err := r.exec("sha256sum " + shellQuote(remotePath))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("sha256sum 没有输出")
	}
	return fields[0], nil
}

// install 解包到目标目录。默认拒绝覆盖正在运行的实例，避免把在线会话拆掉。
func (r *remoteConn) install(tarball, dir, commit string, force bool) (string, error) {
	if !force {
		out, err := r.exec(fmt.Sprintf("pgrep -f %s | head -1 || true", shellQuote("bin/"+commit+"/out/server-main.js")))
		if err == nil {
			if pid := strings.TrimSpace(out); pid != "" {
				return "", fmt.Errorf("该 commit 的 server 正在运行（pid %s），覆盖会打断在线会话；确认无人在用时加 -force", pid)
			}
		}
	}
	// 解包到临时目录再整体换入：避免解到一半失败留下半个 server 目录，
	// 让 VS Code 误以为已安装而反复失败。
	tmp := dir + ".vscpush-tmp"
	cmd := fmt.Sprintf(
		"set -e; rm -rf %[1]s; mkdir -p %[1]s; "+
			"tar -xzf %[2]s -C %[1]s --strip-components=1; "+
			"%[1]s/node --version; "+
			"rm -rf %[3]s; mkdir -p %[3]s; "+
			"if [ -d %[3]s ]; then find %[3]s -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true; fi; "+
			"cp -a %[1]s/. %[3]s/; rm -rf %[1]s",
		shellQuote(tmp), shellQuote(tarball), shellQuote(dir))
	out, err := r.exec(cmd)
	if err != nil {
		return "", err
	}
	return "node " + strings.TrimSpace(out), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func authMethods(c *config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	var candidates []string
	if c.keyFile != "" {
		candidates = []string{c.keyFile}
	} else {
		home, _ := os.UserHomeDir()
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			candidates = append(candidates, filepath.Join(home, ".ssh", n))
		}
	}
	var keyErr string
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			keyErr = fmt.Sprintf("%s: %v", p, err)
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if c.password != "" {
		methods = append(methods, ssh.Password(c.password))
	}
	if len(methods) == 0 {
		if keyErr != "" {
			return nil, fmt.Errorf("没有可用认证方式（私钥解析失败，可能是带口令的密钥：%s）；请用 -key 指定私钥，或设置 VSCPUSH_PASSWORD", keyErr)
		}
		return nil, errors.New("没有可用认证方式：请用 -key 指定私钥、启动 ssh-agent，或设置 VSCPUSH_PASSWORD")
	}
	return methods, nil
}

// preferredHostKeyAlgorithms 让协商顺序优先命中 known_hosts 里已有的密钥类型。
//
// 背景：Go 的默认顺序会先试 ecdsa-sha2-nistp256，而 OpenSSH 客户端优先 ed25519。
// 如果 known_hosts 里只记了 ed25519（很常见，因为平时都用 ssh 连），Go 就会拿 ecdsa
// 去比对，knownhosts 直接报 "key mismatch"——看起来像服务器换了密钥，其实只是算法不同。
// 这里把 known_hosts 里出现过的类型提到前面，行为就和 OpenSSH 一致了。
func preferredHostKeyAlgorithms(c *config) []string {
	known := knownHostKeyTypes(c)
	if len(known) == 0 {
		return nil // 交给 Go 默认顺序
	}
	seen := make(map[string]bool, len(known))
	out := make([]string, 0, len(known)+4)
	for _, t := range known {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	// 其余类型作为后备，避免 known_hosts 里只有一种算法时把别的算法彻底排除。
	for _, t := range []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"} {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// knownHostKeyTypes 读出 known_hosts 中该主机记录过的密钥类型。
func knownHostKeyTypes(c *config) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	khPath := filepath.Join(home, ".ssh", "known_hosts")
	data, err := os.ReadFile(khPath)
	if err != nil {
		return nil
	}
	host := c.host
	if c.port != 22 {
		host = fmt.Sprintf("[%s]:%d", c.host, c.port)
	}
	var types []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !hostMatches(fields[0], host) {
			continue
		}
		types = append(types, fields[1])
	}
	return types
}

// hostMatches 处理 known_hosts 首列的匹配（支持逗号分隔、通配符和哈希记录）。
func hostMatches(pattern, host string) bool {
	if strings.HasPrefix(pattern, "|1|") {
		// 哈希记录无法反解，交给 knownhosts 包自己判断，这里不参与排序。
		return false
	}
	for _, p := range strings.Split(pattern, ",") {
		if p == host {
			return true
		}
		if ok, _ := filepath.Match(p, host); ok {
			return true
		}
	}
	return false
}

// hostKeyCallback 默认采用 accept-new 语义：known_hosts 里有就严格比对，
// 没有就打印指纹后接受；-strict-host-key 时改为拒绝。
func hostKeyCallback(c *config) (ssh.HostKeyCallback, error) {
	home, _ := os.UserHomeDir()
	khPath := filepath.Join(home, ".ssh", "known_hosts")

	var known ssh.HostKeyCallback
	if _, err := os.Stat(khPath); err == nil {
		if cb, err := knownhosts.New(khPath); err == nil {
			known = cb
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if known != nil {
			err := known(hostname, remote, key)
			if err == nil {
				return nil
			}
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) == 0 {
				if c.strictHost {
					return fmt.Errorf("主机密钥不在 known_hosts 中：%s %s（已启用 -strict-host-key）", hostname, fp)
				}
				fmt.Fprintf(os.Stderr, "  提示：首次连接 %s，接受主机密钥 %s\n", hostname, fp)
				return nil
			}
			return err
		}
		if c.strictHost {
			return fmt.Errorf("没有 known_hosts 且启用了 -strict-host-key，拒绝连接 %s（%s）", hostname, fp)
		}
		fmt.Fprintf(os.Stderr, "  提示：本机没有 known_hosts，接受 %s 的主机密钥 %s\n", hostname, fp)
		return nil
	}, nil
}
