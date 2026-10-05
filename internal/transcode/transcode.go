// Package transcode renders formats browsers cannot play (BRSTM, BCSTM,
// BFSTM, ADPCM, ...) to WAV through vgmstream-cli, with a disk cache,
// a bounded worker pool and de-duplication of concurrent requests.
package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info is what vgmstream knows about a stream, including loop points.
type Info struct {
	SampleRate   int    `json:"sampleRate"`
	Channels     int    `json:"channels"`
	TotalSamples int64  `json:"totalSamples"`
	LoopStart    int64  `json:"loopStart"`
	LoopEnd      int64  `json:"loopEnd"`
	HasLoop      bool   `json:"hasLoop"`
	Title        string `json:"title"`
}

// Runner is the decoder backend (vgmstream-cli in production, a fake in tests).
type Runner interface {
	Metadata(ctx context.Context, src string) (Info, error)
	Decode(ctx context.Context, src, dst string) error
}

// ExecFunc runs a command and returns combined output.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// VGMStream drives vgmstream-cli.
type VGMStream struct {
	Bin  string
	Exec ExecFunc // nil means a real subprocess
}

func (v VGMStream) run(ctx context.Context, args ...string) ([]byte, error) {
	ex := v.Exec
	if ex == nil {
		ex = defaultExec
	}
	out, err := ex(ctx, v.Bin, args...)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", v.Bin, err, tail(out))
	}
	return out, nil
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}

// Metadata asks vgmstream for stream info without decoding (-m).
func (v VGMStream) Metadata(ctx context.Context, src string) (Info, error) {
	out, err := v.run(ctx, "-m", src)
	if err != nil {
		return Info{}, err
	}
	return ParseMetadata(string(out))
}

// Decode renders the whole stream once, ignoring its loop (-i), to a WAV file.
// The loop points are delivered separately so the player can loop gaplessly.
func (v VGMStream) Decode(ctx context.Context, src, dst string) error {
	_, err := v.run(ctx, "-i", "-o", dst, src)
	return err
}

var (
	reRate  = regexp.MustCompile(`(?mi)^\s*sample rate:\s*(\d+)`)
	reChan  = regexp.MustCompile(`(?mi)^\s*channels:\s*(\d+)`)
	reTotal = regexp.MustCompile(`(?mi)^\s*stream total samples:\s*(\d+)`)
	reLoopS = regexp.MustCompile(`(?mi)^\s*loop start:\s*(\d+)`)
	reLoopE = regexp.MustCompile(`(?mi)^\s*loop end:\s*(\d+)`)
	reName  = regexp.MustCompile(`(?mi)^\s*stream name:\s*(.+?)\s*$`)
)

func firstInt(re *regexp.Regexp, s string) (int64, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return n, err == nil
}

// ParseMetadata parses the text printed by `vgmstream-cli -m`.
func ParseMetadata(out string) (Info, error) {
	var in Info
	rate, ok := firstInt(reRate, out)
	if !ok || rate <= 0 {
		return Info{}, errors.New("vgmstream reported no sample rate")
	}
	in.SampleRate = int(rate)
	if n, ok := firstInt(reChan, out); ok {
		in.Channels = int(n)
	}
	if n, ok := firstInt(reTotal, out); ok {
		in.TotalSamples = n
	}
	s, okS := firstInt(reLoopS, out)
	e, okE := firstInt(reLoopE, out)
	if okS && okE && e > s {
		in.HasLoop, in.LoopStart, in.LoopEnd = true, s, e
	}
	if m := reName.FindStringSubmatch(out); m != nil {
		in.Title = m[1]
	}
	return in, nil
}

type call struct {
	done chan struct{}
	path string
	err  error
}

// Service caches rendered files and limits concurrent decodes.
type Service struct {
	run      Runner
	cacheDir string
	maxCache int64
	timeout  time.Duration
	sem      chan struct{}

	mu       sync.Mutex
	inflight map[string]*call
	infos    map[string]Info
}

// New creates the cache directory and clears stale partial files.
func New(run Runner, cacheDir string, workers int, maxCacheBytes int64) (*Service, error) {
	if workers < 1 {
		workers = 1
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	if parts, err := filepath.Glob(filepath.Join(cacheDir, "*.part")); err == nil {
		for _, p := range parts {
			os.Remove(p)
		}
	}
	return &Service{
		run:      run,
		cacheDir: cacheDir,
		maxCache: maxCacheBytes,
		timeout:  5 * time.Minute,
		sem:      make(chan struct{}, workers),
		inflight: map[string]*call{},
		infos:    map[string]Info{},
	}, nil
}

func (s *Service) key(src string) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", src, st.Size(), st.ModTime().UnixNano())))
	return hex.EncodeToString(sum[:16]), nil
}

// Info returns (and caches) stream info for src.
func (s *Service) Info(ctx context.Context, src string) (Info, error) {
	key, err := s.key(src)
	if err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	in, ok := s.infos[key]
	s.mu.Unlock()
	if ok {
		return in, nil
	}
	in, err = s.run.Metadata(ctx, src)
	if err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	s.infos[key] = in
	s.mu.Unlock()
	return in, nil
}

// Render returns the path of a cached WAV for src, decoding it if needed.
// Concurrent requests for the same file share one decode. A caller that goes
// away does not cancel the decode, so the next request benefits from it.
func (s *Service) Render(ctx context.Context, src string) (string, error) {
	key, err := s.key(src)
	if err != nil {
		return "", err
	}
	out := filepath.Join(s.cacheDir, key+".wav")
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		now := time.Now()
		_ = os.Chtimes(out, now, now) // keeps the cache LRU
		return out, nil
	}
	s.mu.Lock()
	c, ok := s.inflight[key]
	if !ok {
		c = &call{done: make(chan struct{})}
		s.inflight[key] = c
		go s.work(key, src, out, c)
	}
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.path, c.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Prefetch starts rendering src in the background.
func (s *Service) Prefetch(src string) {
	go func() { _, _ = s.Render(context.Background(), src) }()
}

func (s *Service) work(key, src, out string, c *call) {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		close(c.done)
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	part := out + ".part"
	if err := s.run.Decode(ctx, src, part); err != nil {
		os.Remove(part)
		c.err = err
		return
	}
	if st, err := os.Stat(part); err != nil || st.Size() == 0 {
		os.Remove(part)
		c.err = errors.New("decoder produced no output")
		return
	}
	if err := os.Rename(part, out); err != nil {
		os.Remove(part)
		c.err = err
		return
	}
	c.path = out
	s.trim()
}

// trim deletes the least recently used renders until the cache fits.
func (s *Service) trim() {
	if s.maxCache <= 0 {
		return
	}
	des, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".wav") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(s.cacheDir, de.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for i := 0; i < len(items)-1 && total > s.maxCache; i++ { // always keep the newest
		if os.Remove(items[i].path) == nil {
			total -= items[i].size
		}
	}
}
