package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// downloadRanged 把文件切成 conns 段，每段独立重试；进度按已落盘字节累计。
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

	chunk := (rf.size + int64(conns) - 1) / int64(conns)
	if chunk < 1 {
		chunk = 1
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for i := 0; i < conns; i++ {
		start := int64(i) * chunk
		if start >= rf.size {
			break
		}
		end := start + chunk - 1
		if end >= rf.size {
			end = rf.size - 1
		}
		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			if err := fetchChunk(ctx, rf.url, f, start, end, pr); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
		}(start, end)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func fetchChunk(ctx context.Context, url string, f *os.File, start, end int64, pr *progress) error {
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
