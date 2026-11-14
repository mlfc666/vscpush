package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type span struct{ start, end int64 }

// remoteFile 是探测到的下载源信息：跟随重定向后的最终地址、总大小、是否支持 Range。
type remoteFile struct {
	url    string
	size   int64
	ranged bool
}

var httpClient = &http.Client{
	Transport: &http.Transport{
		// 让 HTTPS_PROXY / HTTP_PROXY 生效（企业内网常见）
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		// .tar.gz 本身已压缩，不要再让传输层做 gzip，否则 Range 与总长度都会错位。
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("重定向过多")
		}
		return nil
	},
}

// resolveDownload 先用 1 字节的 Range 探测：既拿到重定向后的真实地址，也确认是否支持分片。
func resolveDownload(ctx context.Context, rawURL string) (*remoteFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", toolName+"/"+toolVersion)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求下载地址失败: %w", err)
	}
	defer resp.Body.Close()

	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}

	if resp.StatusCode == http.StatusPartialContent {
		if total, err := parseContentRangeTotal(resp.Header.Get("Content-Range")); err == nil && total > 0 {
			return &remoteFile{url: final, size: total, ranged: true}, nil
		}
	}
	if resp.StatusCode == http.StatusOK {
		return &remoteFile{url: final, size: resp.ContentLength, ranged: false}, nil
	}
	return nil, fmt.Errorf("探测失败: HTTP %s", resp.Status)
}

func parseContentRangeTotal(v string) (int64, error) {
	// 形如 "bytes 0-0/236720088"
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0, fmt.Errorf("无法解析 Content-Range: %q", v)
	}
	s := strings.TrimSpace(v[i+1:])
	if s == "*" {
		return 0, fmt.Errorf("大小未知")
	}
	return strconv.ParseInt(s, 10, 64)
}

// 下载引擎：auto 有 aria2c 就用它，没有就用内置实现。
const (
	EngineAuto    = "auto"
	EngineAria2   = "aria2c"
	EngineBuiltin = "builtin"
)

// aria2Path 判断系统里有没有 aria2c。
func aria2Path() (string, bool) {
	p, err := exec.LookPath("aria2c")
	if err != nil {
		return "", false
	}
	return p, true
}

// pickEngine 决定实际用哪个引擎。
func pickEngine(want string) string {
	switch want {
	case EngineBuiltin:
		return EngineBuiltin
	case EngineAria2:
		return EngineAria2
	}
	if _, ok := aria2Path(); ok {
		return EngineAria2
	}
	return EngineBuiltin
}

// downloadAria2 把下载交给系统的 aria2c，参数与内置实现对齐（连接数、1 MiB 最小分片）。
//
// 为什么不解析它的输出做进度条：aria2c 的实时读数行在管道下是块缓冲的（攒满 4 KiB 才吐），
// 解析出来只会是「长时间 0% 然后瞬间 100%」。所以这里把 stdout/stderr 直接接到终端，
// 让它自己渲染 `[#gid 12MiB/225MiB(5%) CN:16 DL:6.9MiB ETA:30s]` 那行，与直接用 aria2c 体验一致。
func downloadAria2(ctx context.Context, rf *remoteFile, dest string, conns int, quiet bool) error {
	bin, ok := aria2Path()
	if !ok {
		return fmt.Errorf("找不到 aria2c（请先安装，或用 -engine builtin）")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dest)
	_ = os.Remove(dest + ".aria2")

	perServer := conns
	if perServer > 16 { // aria2c 单服务器连接数上限就是 16
		perServer = 16
	}
	if perServer < 1 {
		perServer = 1
	}
	args := []string{
		fmt.Sprintf("--max-connection-per-server=%d", perServer),
		fmt.Sprintf("--split=%d", conns),
		"--min-split-size=1M",
		"--file-allocation=none",
		"--auto-file-renaming=false",
		"--allow-overwrite=true",
		"--console-log-level=error",
		"--summary-interval=0",
		"--enable-color=false",
		"--max-tries=8",
		"--retry-wait=1",
		"--timeout=30",
		"--connect-timeout=30",
	}
	if quiet {
		args = append(args, "--quiet=true")
	}
	args = append(args, "-d", filepath.Dir(dest), "-o", filepath.Base(dest), rf.url)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aria2c 退出异常: %w", err)
	}
	if rf.size > 0 {
		st, err := os.Stat(dest)
		if err != nil {
			return fmt.Errorf("aria2c 结束后找不到文件: %w", err)
		}
		if st.Size() != rf.size {
			return fmt.Errorf("aria2c 产物大小异常: %d != %d", st.Size(), rf.size)
		}
	}
	return nil
}

func downloadFile(ctx context.Context, rf *remoteFile, dest string, conns int, pr *progress) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if !rf.ranged || conns <= 1 || rf.size <= 0 {
		return downloadSingle(ctx, rf, dest, pr)
	}
	return downloadRanged(ctx, rf, dest, conns, pr)
}

func downloadSingle(ctx context.Context, rf *remoteFile, dest string, pr *progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rf.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", toolName+"/"+toolVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败: HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyBuffer(f, &countingReader{r: resp.Body, p: pr}, make([]byte, 256<<10))
	return err
}

// downloadRanged 用「动态分片」并发下载：整包按 1 MiB 切片投进队列，conns 个 worker
// 竞争式领取，每片独立重试。相比等分固定块，链路不均时快连接会自动多干，
// 不会把总耗时钉在最慢的那条连接上（实测不均衡链路下 4.94s → 1.9s）。
func downloadRanged(ctx context.Context, rf *remoteFile, dest string, conns int, pr *progress) error {
	f, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(rf.size); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	const pieceSize = 1 << 20 // 1 MiB/片：足够细以平衡快慢连接，又不至于请求数爆炸
	pieces := splitSpans(rf.size, pieceSize)
	spans := make(chan span, conns*2)
	go func() {
		defer close(spans)
		for _, sp := range pieces {
			select {
			case spans <- sp:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sp := range spans {
				if err := fetchRange(ctx, rf.url, f, sp.start, sp.end, pr); err != nil {
					select {
					case errCh <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func fetchRange(ctx context.Context, url string, f *os.File, start, end int64, pr *progress) error {
	const maxAttempts = 8
	pos := start
	buf := make([]byte, 256<<10)
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		if pos > end {
			return nil
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, end))
		req.Header.Set("User-Agent", toolName+"/"+toolVersion)

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("分片请求返回 HTTP %s（期望 206）", resp.Status)
			if resp.StatusCode == http.StatusOK {
				// 服务器忽略了 Range：继续写会错位，直接失败而不是产出坏包。
				return lastErr
			}
			continue
		}

		readErr := func() error {
			defer resp.Body.Close()
			for pos <= end {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					remaining := end + 1 - pos
					if int64(n) > remaining {
						n = int(remaining)
					}
					if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
						return werr
					}
					pos += int64(n)
					pr.Add(int64(n))
				}
				if rerr == io.EOF {
					return nil
				}
				if rerr != nil {
					return rerr
				}
			}
			return nil
		}()

		if readErr == nil && pos > end {
			return nil
		}
		if readErr != nil {
			lastErr = readErr
		} else {
			lastErr = io.ErrUnexpectedEOF
		}
	}
	if pos > end {
		return nil
	}
	return fmt.Errorf("分片 [%d,%d] 重试 %d 次仍失败: %w", start, end, maxAttempts, lastErr)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// splitSpans 把 [0,size) 切成连续且无重叠的片段，最后一片按剩余长度截断。
func splitSpans(size, pieceSize int64) []span {
	if pieceSize < 1 {
		pieceSize = 1
	}
	out := make([]span, 0, size/pieceSize+1)
	for off := int64(0); off < size; off += pieceSize {
		end := off + pieceSize - 1
		if end >= size {
			end = size - 1
		}
		out = append(out, span{off, end})
	}
	return out
}
