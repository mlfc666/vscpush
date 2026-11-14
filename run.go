package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// run 串起整条流水线：连远端 → 探架构 → 并发下载 → SFTP 上传 → 远端校验 →（可选）解包安装。
func run(ctx context.Context, c *config) error {
	t0 := time.Now()
	fmt.Printf("▸ %s v%s\n", toolName, toolVersion)

	rc, err := dialRemote(c)
	if err != nil {
		return err
	}
	defer rc.Close()
	fmt.Printf("  已连接 %s@%s:%d，家目录 %s\n", c.user, c.host, c.port, rc.home)

	if c.arch == "auto" {
		a, err := rc.detectArch()
		if err != nil {
			return fmt.Errorf("自动探测远端架构失败: %w", err)
		}
		c.arch = a
		fmt.Printf("  远端架构 %s（自动探测），对应服务端包 linux-%s\n", rc.unameArch, c.arch)
	} else if err := checkArch(c.arch); err != nil {
		return err
	}

	name := c.remoteName
	if name == "" {
		name = fmt.Sprintf("vscode-server-linux-%s.tar.gz", c.arch)
	}
	dlURL := fmt.Sprintf("https://update.code.visualstudio.com/commit:%s/server-linux-%s/stable", c.commit, c.arch)
	localPath := filepath.Join(c.localDir, name)
	remotePath := rc.absPath(path.Join(c.remoteDir, name))

	installDir := c.installDir
	if installDir == "" {
		installDir = path.Join(".vscode-server", "bin", c.commit)
	}
	installDir = rc.absPath(installDir)

	fmt.Printf("  下载地址 %s\n", dlURL)
	fmt.Printf("  本地文件 %s\n", localPath)
	fmt.Printf("  远端文件 %s:%s\n", c.host, remotePath)
	if c.install {
		fmt.Printf("  安装目录 %s（上传后解包）\n", installDir)
	}

	fmt.Printf("\n▸ 探测下载源 …\n")
	rf, err := resolveDownload(ctx, dlURL)
	if err != nil {
		return err
	}
	fmt.Printf("  大小 %s，支持分片 %v，最终地址已跟随重定向\n", humanBytes(rf.size), rf.ranged)

	if c.dryRun {
		fmt.Printf("\n(dry-run) 计划如上，未下载、未上传。\n")
		return nil
	}

	fmt.Printf("\n▸ 下载（%d 连接并发）\n", c.conns)
	pr := newProgress("下载", rf.size)
	pr.Start()
	dlStart := time.Now()
	err = downloadFile(ctx, rf, localPath, c.conns, pr)
	pr.Stop()
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	dlDur := time.Since(dlStart)

	sha, err := fileSHA256(localPath)
	if err != nil {
		return err
	}
	fmt.Printf("  sha256 %s\n", sha)

	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if rf.size > 0 && info.Size() != rf.size {
		return fmt.Errorf("下载文件大小异常: 本地 %d != 预期 %d", info.Size(), rf.size)
	}

	fmt.Printf("\n▸ 上传到 %s:%s\n", c.host, remotePath)
	up := newProgress("上传", info.Size())
	up.Start()
	upStart := time.Now()
	err = rc.upload(localPath, remotePath, up)
	up.Stop()
	if err != nil {
		return fmt.Errorf("上传失败: %w", err)
	}
	upDur := time.Since(upStart)

	if !c.noVerify {
		fmt.Printf("\n▸ 远端校验 sha256 …\n")
		remoteSha, err := rc.remoteSHA256(remotePath)
		if err != nil {
			return fmt.Errorf("远端校验失败: %w", err)
		}
		if remoteSha != sha {
			return fmt.Errorf("传输损坏：本地 %s ≠ 远端 %s", sha, remoteSha)
		}
		fmt.Printf("  一致 %s\n", sha)
	}

	if c.install {
		fmt.Printf("\n▸ 解包安装到 %s\n", installDir)
		out, err := rc.install(remotePath, installDir, c.commit, c.force)
		if err != nil {
			return fmt.Errorf("安装失败: %w", err)
		}
		fmt.Printf("  完成：%s\n", strings.TrimSpace(out))
	}

	fmt.Printf("\n✅ 完成，总耗时 %s\n", fmtDur(time.Since(t0)))
	fmt.Printf("   下载  %s  %s/s  %s\n", humanBytes(info.Size()), humanBytes(rate(info.Size(), dlDur)), fmtDur(dlDur))
	fmt.Printf("   上传  %s  %s/s  %s\n", humanBytes(info.Size()), humanBytes(rate(info.Size(), upDur)), fmtDur(upDur))
	fmt.Printf("   远端  %s@%s:%s\n", c.user, c.host, remotePath)
	return nil
}

func rate(n int64, d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(float64(n) / d.Seconds())
}

func checkArch(a string) error {
	switch a {
	case "x64", "arm64", "armhf":
		return nil
	}
	return fmt.Errorf("不支持的 -arch: %q（可选 auto / x64 / arm64 / armhf）", a)
}
