package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var quietProgress bool

// progress 是一个对 TTY 友好的单行进度条：并发下载时多 goroutine 只做原子累加，
// 渲染统一交给后台 ticker，避免互相抢占 stderr。
type progress struct {
	label  string
	total  int64
	done   atomic.Int64
	tty    bool
	stop   chan struct{}
	fin    chan struct{}
	mu     sync.Mutex
	speed  float64
	lastTs time.Time
}

func newProgress(label string, total int64) *progress {
	p := &progress{label: label, total: total, stop: make(chan struct{}), fin: make(chan struct{})}
	if fi, err := os.Stderr.Stat(); err == nil {
		p.tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return p
}

func (p *progress) Add(n int64) { p.done.Add(n) }

func (p *progress) Start() {
	if quietProgress {
		go func() { <-p.stop; close(p.fin) }()
		return
	}
	go p.loop()
}

func (p *progress) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.fin
	if p.tty && !quietProgress {
		fmt.Fprintln(os.Stderr)
	}
}

func (p *progress) loop() {
	defer close(p.fin)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	last := time.Now()
	lastDone := p.done.Load()
	for {
		select {
		case <-p.stop:
			p.render(p.done.Load(), p.currentSpeed())
			return
		case now := <-ticker.C:
			d := p.done.Load()
			if dt := now.Sub(last).Seconds(); dt > 0 {
				inst := float64(d-lastDone) / dt
				p.mu.Lock()
				// 指数平滑，避免并发分片的抖动让速度数字乱跳
				p.speed = p.speed*0.75 + inst*0.25
				p.mu.Unlock()
			}
			last, lastDone = now, d
			if p.tty {
				p.render(d, p.currentSpeed())
			} else if now.Sub(p.lastTs) > 5*time.Second {
				p.lastTs = now
				p.render(d, p.currentSpeed())
			}
		}
	}
}

func (p *progress) currentSpeed() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.speed
}

func (p *progress) render(done int64, speed float64) {
	pct := 0.0
	total := "?"
	if p.total > 0 {
		pct = float64(done) / float64(p.total) * 100
		total = humanBytes(p.total)
	}
	eta := "--:--"
	if speed > 1024 && p.total > done {
		eta = fmtDur(time.Duration(float64(p.total-done) / speed * float64(time.Second)))
	}
	line := fmt.Sprintf("%s  %s / %s  %5.1f%%  %s/s  ETA %s",
		p.label, humanBytes(done), total, pct, humanBytes(int64(speed)), eta)
	if p.tty {
		fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
	} else {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
}

// countingReader 把读取量喂给进度条，单流下载和上传共用。
type countingReader struct {
	r io.Reader
	p *progress
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 && c.p != nil {
		c.p.Add(int64(n))
	}
	return n, err
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}
