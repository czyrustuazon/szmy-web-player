package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sampleMeta = `metadata for song.brstm
encoding: Nintendo DSP ADPCM
layout: interleave
sample rate: 32000 Hz
channels: 2
loop start: 12345 samples (0:00.38)
loop end: 2345678 samples (1:13.30)
stream total samples: 2400000 (1:15.00)
stream name: Boss Fight
`

func TestParseMetadata(t *testing.T) {
	in, err := ParseMetadata(sampleMeta)
	if err != nil {
		t.Fatal(err)
	}
	want := Info{SampleRate: 32000, Channels: 2, TotalSamples: 2400000, LoopStart: 12345, LoopEnd: 2345678, HasLoop: true, Title: "Boss Fight"}
	if in != want {
		t.Fatalf("got %+v want %+v", in, want)
	}
}

func TestParseMetadataWithoutLoopOrName(t *testing.T) {
	in, err := ParseMetadata("sample rate: 44100 Hz\nchannels: 1\nstream total samples: 100 (0:00.00)\n")
	if err != nil || in.HasLoop || in.Title != "" || in.Channels != 1 || in.TotalSamples != 100 {
		t.Fatalf("got %+v %v", in, err)
	}
	// A loop end that is not after the loop start is ignored.
	in, _ = ParseMetadata("sample rate: 44100 Hz\nloop start: 500 samples\nloop end: 100 samples\n")
	if in.HasLoop {
		t.Errorf("inverted loop: %+v", in)
	}
	// Only one of the two loop lines present.
	in, _ = ParseMetadata("sample rate: 44100 Hz\nloop start: 5 samples\n")
	if in.HasLoop {
		t.Errorf("half loop: %+v", in)
	}
}

func TestParseMetadataErrors(t *testing.T) {
	for _, s := range []string{"", "nothing useful", "sample rate: 0 Hz"} {
		if _, err := ParseMetadata(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestVGMStreamCommands(t *testing.T) {
	var gotName string
	var gotArgs []string
	v := VGMStream{Bin: "/opt/vgmstream-cli", Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(sampleMeta), nil
	}}
	in, err := v.Metadata(context.Background(), "/m/song.brstm")
	if err != nil || in.SampleRate != 32000 {
		t.Fatalf("metadata: %+v %v", in, err)
	}
	if gotName != "/opt/vgmstream-cli" || strings.Join(gotArgs, " ") != "-m /m/song.brstm" {
		t.Fatalf("metadata command: %s %v", gotName, gotArgs)
	}
	if err := v.Decode(context.Background(), "/m/song.brstm", "/c/out.wav.part"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "-i -o /c/out.wav.part /m/song.brstm" {
		t.Fatalf("decode command: %v", gotArgs)
	}
}

func TestVGMStreamErrorIncludesOutputTail(t *testing.T) {
	v := VGMStream{Bin: "vgm", Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(strings.Repeat("x", 500) + "unsupported codec"), errors.New("exit status 1")
	}}
	_, err := v.Metadata(context.Background(), "a")
	if err == nil || !strings.Contains(err.Error(), "unsupported codec") || len(err.Error()) > 500 {
		t.Fatalf("got %v", err)
	}
	if err := v.Decode(context.Background(), "a", "b"); err == nil {
		t.Error("decode error")
	}
	bad := VGMStream{Bin: "vgm", Exec: func(context.Context, string, ...string) ([]byte, error) { return []byte("garbage"), nil }}
	if _, err := bad.Metadata(context.Background(), "a"); err == nil {
		t.Error("unparseable metadata")
	}
}

func TestDefaultExecRunsProcesses(t *testing.T) {
	if _, err := defaultExec(context.Background(), "definitely-not-a-real-binary-xyz"); err == nil {
		t.Error("missing binary should fail")
	}
}

// fakeRunner decodes by writing a small file; optionally blocks to test sharing.
type fakeRunner struct {
	decodes  int32
	metas    int32
	gate     chan struct{}
	failWith error
	empty    bool
	info     Info
}

func (f *fakeRunner) Metadata(ctx context.Context, src string) (Info, error) {
	atomic.AddInt32(&f.metas, 1)
	if f.failWith != nil {
		return Info{}, f.failWith
	}
	return f.info, nil
}

func (f *fakeRunner) Decode(ctx context.Context, src, dst string) error {
	atomic.AddInt32(&f.decodes, 1)
	if f.gate != nil {
		<-f.gate
	}
	if f.failWith != nil {
		return f.failWith
	}
	data := []byte("RIFFfakewav")
	if f.empty {
		data = nil
	}
	return os.WriteFile(dst, data, 0o644)
}

func newService(t *testing.T, r Runner, max int64) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(r, filepath.Join(dir, "cache"), 2, max)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "song.brstm")
	if err := os.WriteFile(src, []byte("RSTM"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, src
}

func TestRenderCachesResult(t *testing.T) {
	r := &fakeRunner{}
	s, src := newService(t, r, 0)
	p1, err := s.Render(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p1); string(data) != "RIFFfakewav" {
		t.Fatalf("content %q", data)
	}
	p2, err := s.Render(context.Background(), src)
	if err != nil || p2 != p1 || atomic.LoadInt32(&r.decodes) != 1 {
		t.Fatalf("second render: %q %v decodes=%d", p2, err, r.decodes)
	}
	// Changing the source invalidates the cache entry.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(src, []byte("RSTM-new-and-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	p3, err := s.Render(context.Background(), src)
	if err != nil || p3 == p1 || atomic.LoadInt32(&r.decodes) != 2 {
		t.Fatalf("after change: %q %v decodes=%d", p3, err, r.decodes)
	}
}

func TestConcurrentRendersShareOneDecode(t *testing.T) {
	r := &fakeRunner{gate: make(chan struct{})}
	s, src := newService(t, r, 0)
	var wg sync.WaitGroup
	paths := make([]string, 8)
	errs := make([]error, 8)
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = s.Render(context.Background(), src)
		}(i)
	}
	// Wait until the single decode has started, then let everyone pile up before releasing it.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&r.decodes) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	close(r.gate)
	wg.Wait()
	for i := range paths {
		if errs[i] != nil || paths[i] != paths[0] {
			t.Fatalf("caller %d: %q %v", i, paths[i], errs[i])
		}
	}
	if n := atomic.LoadInt32(&r.decodes); n != 1 {
		t.Fatalf("expected 1 decode, got %d", n)
	}
}

func TestRenderCallerCancelDoesNotAbortDecode(t *testing.T) {
	r := &fakeRunner{gate: make(chan struct{})}
	s, src := newService(t, r, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Render(ctx, src); done <- err }()
	for atomic.LoadInt32(&r.decodes) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	close(r.gate)
	// The decode finishes in the background; the next request is served from cache.
	var p string
	var err error
	for i := 0; i < 200; i++ {
		p, err = s.Render(context.Background(), src)
		if err == nil && atomic.LoadInt32(&r.decodes) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || p == "" || atomic.LoadInt32(&r.decodes) != 1 {
		t.Fatalf("got %q %v decodes=%d", p, err, r.decodes)
	}
}

func TestRenderFailures(t *testing.T) {
	r := &fakeRunner{failWith: errors.New("bad codec")}
	s, src := newService(t, r, 0)
	if _, err := s.Render(context.Background(), src); err == nil || !strings.Contains(err.Error(), "bad codec") {
		t.Fatalf("decode failure: %v", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(s.cacheDir, "*")); len(parts) != 0 {
		t.Errorf("failed decode left files: %v", parts)
	}
	// A retry is allowed after a failure.
	r.failWith = nil
	if _, err := s.Render(context.Background(), src); err != nil {
		t.Fatalf("retry: %v", err)
	}

	r2 := &fakeRunner{empty: true}
	s2, src2 := newService(t, r2, 0)
	if _, err := s2.Render(context.Background(), src2); err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("empty output: %v", err)
	}
	if _, err := s2.Render(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing source")
	}
}

func TestInfoIsCached(t *testing.T) {
	r := &fakeRunner{info: Info{SampleRate: 44100, HasLoop: true, LoopStart: 1, LoopEnd: 9}}
	s, src := newService(t, r, 0)
	for i := 0; i < 3; i++ {
		in, err := s.Info(context.Background(), src)
		if err != nil || in.LoopEnd != 9 {
			t.Fatalf("%+v %v", in, err)
		}
	}
	if atomic.LoadInt32(&r.metas) != 1 {
		t.Fatalf("metadata should be cached, calls=%d", r.metas)
	}
	if _, err := s.Info(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing source")
	}
	r.failWith = errors.New("nope")
	other := filepath.Join(filepath.Dir(src), "other.brstm")
	_ = os.WriteFile(other, []byte("RSTM"), 0o644)
	if _, err := s.Info(context.Background(), other); err == nil {
		t.Error("runner failure")
	}
}

func TestPrefetchWarmsCache(t *testing.T) {
	r := &fakeRunner{}
	s, src := newService(t, r, 0)
	s.Prefetch(src)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if files, _ := filepath.Glob(filepath.Join(s.cacheDir, "*.wav")); len(files) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("prefetch did not produce a cached file")
}

func TestTrimEvictsOldestButKeepsNewest(t *testing.T) {
	r := &fakeRunner{}
	s, _ := newService(t, r, 30) // room for two 11-byte files
	now := time.Now()
	for i, name := range []string{"a.wav", "b.wav", "c.wav", "d.wav"} {
		p := filepath.Join(s.cacheDir, name)
		if err := os.WriteFile(p, []byte("RIFFfakewav"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	_ = os.WriteFile(filepath.Join(s.cacheDir, "note.txt"), []byte("ignored"), 0o644)
	s.trim()
	left, _ := filepath.Glob(filepath.Join(s.cacheDir, "*.wav"))
	if len(left) != 2 {
		t.Fatalf("expected 2 files left, got %v", left)
	}
	for _, f := range left {
		if base := filepath.Base(f); base != "c.wav" && base != "d.wav" {
			t.Errorf("oldest files should go first, kept %s", base)
		}
	}

	// A single file larger than the whole budget is still kept.
	s2, _ := newService(t, r, 5)
	p := filepath.Join(s2.cacheDir, "big.wav")
	_ = os.WriteFile(p, []byte("RIFFfakewav"), 0o644)
	s2.trim()
	if _, err := os.Stat(p); err != nil {
		t.Error("newest file must never be evicted")
	}

	// Unlimited cache and missing directory are no-ops.
	s3, _ := newService(t, r, 0)
	s3.trim()
	s3.maxCache = 1
	s3.cacheDir = filepath.Join(t.TempDir(), "gone")
	s3.trim()
}

func TestNewClearsStalePartFilesAndValidates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	_ = os.MkdirAll(dir, 0o755)
	part := filepath.Join(dir, "x.wav.part")
	_ = os.WriteFile(part, []byte("half"), 0o644)
	s, err := New(&fakeRunner{}, dir, 0, 0) // workers < 1 is bumped to 1
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error("stale .part file should be removed")
	}
	if cap(s.sem) != 1 {
		t.Errorf("workers: %d", cap(s.sem))
	}
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o644)
	if _, err := New(&fakeRunner{}, filepath.Join(blocker, "cache"), 1, 0); err == nil {
		t.Error("cache dir under a file")
	}
}

func TestWorkerPoolLimitsConcurrency(t *testing.T) {
	var running, peak int32
	r := &countingRunner{running: &running, peak: &peak}
	dir := t.TempDir()
	s, err := New(r, filepath.Join(dir, "cache"), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		src := filepath.Join(dir, strings.Repeat("s", i+1)+".brstm")
		_ = os.WriteFile(src, []byte("RSTM"), 0o644)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Render(context.Background(), src); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt32(&peak); p > 2 || p < 1 {
		t.Fatalf("peak concurrency %d, want 1..2", p)
	}
}

type countingRunner struct{ running, peak *int32 }

func (c *countingRunner) Metadata(context.Context, string) (Info, error) { return Info{}, nil }

func (c *countingRunner) Decode(ctx context.Context, src, dst string) error {
	n := atomic.AddInt32(c.running, 1)
	for {
		p := atomic.LoadInt32(c.peak)
		if n <= p || atomic.CompareAndSwapInt32(c.peak, p, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	atomic.AddInt32(c.running, -1)
	return os.WriteFile(dst, []byte("RIFFx"), 0o644)
}
