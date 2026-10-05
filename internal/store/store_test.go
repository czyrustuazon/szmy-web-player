package store

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestDefaultsOnFreshStore(t *testing.T) {
	s, _ := open(t)
	if got := s.Settings(); got != DefaultSettings() {
		t.Fatalf("settings: %+v", got)
	}
	if len(s.Favorites()) != 0 || s.Resume() != (Resume{}) {
		t.Fatal("fresh store should be empty")
	}
}

func TestFavoritesPersistAndOrder(t *testing.T) {
	s, p := open(t)
	base := time.Unix(1000, 0)
	tick := 0
	s.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }

	for _, path := range []string{"a.mp3", "b.mp3", "c.mp3"} {
		if err := s.SetFavorite(path, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetFavorite("b.mp3", true); err != nil { // already a favorite: no change
		t.Fatal(err)
	}
	if err := s.SetFavorite("zzz.mp3", false); err != nil { // not a favorite: no change
		t.Fatal(err)
	}
	if err := s.SetFavorite("a.mp3", false); err != nil {
		t.Fatal(err)
	}
	favs := s.Favorites()
	if len(favs) != 2 || favs[0].Path != "c.mp3" || favs[1].Path != "b.mp3" {
		t.Fatalf("newest first expected: %+v", favs)
	}
	if !s.IsFavorite("c.mp3") || s.IsFavorite("a.mp3") {
		t.Fatal("IsFavorite")
	}

	again, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Favorites(); len(got) != 2 || got[0].Path != "c.mp3" {
		t.Fatalf("persisted: %+v", got)
	}
}

func TestFavoritesTieBreakByPath(t *testing.T) {
	s, _ := open(t)
	s.now = func() time.Time { return time.Unix(5, 0) }
	_ = s.SetFavorite("b", true)
	_ = s.SetFavorite("a", true)
	if f := s.Favorites(); f[0].Path != "a" {
		t.Fatalf("ties sort by path: %+v", f)
	}
}

func TestSettingsNormalizeAndPersist(t *testing.T) {
	s, p := open(t)
	got, err := s.SetSettings(Settings{
		Shuffle: true, Repeat: "weird", Volume: 7, LoopMode: "???", LoopCount: 99,
		FadeSeconds: -3, VizMode: "disco", SkipGuard: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Shuffle: true, Repeat: "off", Volume: 1, LoopMode: "count", LoopCount: 20, FadeSeconds: 0, VizMode: "bars", SkipGuard: true}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	again, _ := Open(p)
	if again.Settings() != want {
		t.Fatalf("persisted: %+v", again.Settings())
	}
}

func TestNormalizeBounds(t *testing.T) {
	cases := []struct {
		in   Settings
		want Settings
	}{
		{Settings{Repeat: "one", LoopMode: "forever", VizMode: "scope", Volume: 0.5, LoopCount: 3, FadeSeconds: 30}, Settings{Repeat: "one", LoopMode: "forever", VizMode: "scope", Volume: 0.5, LoopCount: 3, FadeSeconds: 30}},
		{Settings{Repeat: "all", LoopMode: "once", VizMode: "off", Volume: -1, LoopCount: 0, FadeSeconds: 31}, Settings{Repeat: "all", LoopMode: "once", VizMode: "off", Volume: 0, LoopCount: 2, FadeSeconds: 30}},
	}
	for i, c := range cases {
		if got := c.in.Normalize(); got != c.want {
			t.Errorf("case %d: got %+v want %+v", i, got, c.want)
		}
	}
	nan := Settings{Repeat: "off", LoopMode: "count", VizMode: "bars", LoopCount: 1}
	nan.Volume = nan.Volume / nan.Volume // NaN (0/0)
	if v := nan.Normalize().Volume; v != 0 {
		t.Errorf("NaN volume should become 0, got %v", v)
	}
}

func TestResume(t *testing.T) {
	s, p := open(t)
	if err := s.SetResume(Resume{Path: "a/b.mp3", Position: 12.5, Source: "favorites"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResume(Resume{Path: "x", Position: -4, Source: "bogus"}); err != nil {
		t.Fatal(err)
	}
	if r := s.Resume(); r.Position != 0 || r.Source != "library" || r.Path != "x" {
		t.Fatalf("normalised: %+v", r)
	}
	_ = s.SetResume(Resume{Path: "a/b.mp3", Position: 12.5, Source: "favorites"})
	again, _ := Open(p)
	if r := again.Resume(); r.Path != "a/b.mp3" || r.Position != 12.5 || r.Source != "favorites" {
		t.Fatalf("persisted: %+v", r)
	}
}

// state.json files written by earlier versions may still hold a password hash; it is ignored.
func TestOpenIgnoresLegacyPasswordFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(p, []byte(`{"passSalt":"ab","passHash":"cd","favorites":{"a.mp3":1}}`), 0o644)
	s, err := Open(p)
	if err != nil || !s.IsFavorite("a.mp3") {
		t.Fatalf("legacy file should load: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := Open(bad); err == nil {
		t.Error("corrupt file must be reported, not silently reset")
	}
	// A path that is a directory cannot be read as a file.
	if _, err := Open(dir); err == nil {
		t.Error("directory as state file")
	}
}

func TestOpenAcceptsPartialAndNullFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(p, []byte(`{"favorites": null, "settings": {"volume": 0.25}}`), 0o644)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite("a", true); err != nil {
		t.Fatalf("nil favorites map must be usable: %v", err)
	}
	st := s.Settings()
	if st.Volume != 0.25 || st.Repeat != "off" || st.LoopCount != 2 {
		t.Fatalf("missing fields should take defaults: %+v", st)
	}
}

func TestSaveReportsEncodingAndWriteFailures(t *testing.T) {
	s, p := open(t)
	if err := s.SetFavorite("a", true); err != nil {
		t.Fatal(err)
	}

	// A value JSON cannot encode (NaN) is reported instead of corrupting the file.
	s.st.Settings.Volume = math.NaN()
	if err := s.save(); err == nil {
		t.Error("NaN volume should fail to encode")
	}
	s.st.Settings.Volume = 1

	// A directory squatting on the temp file name makes the write fail.
	if err := os.Mkdir(p+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite("b", true); err == nil {
		t.Error("blocked temp file should fail the save")
	}
}

func TestSaveFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o644)
	// Built directly: how Open treats an unreadable parent differs per OS.
	s := &Store{path: filepath.Join(blocker, "state.json"), st: state{Favorites: map[string]int64{}}, now: time.Now}
	if err := s.SetFavorite("a", true); err == nil {
		t.Error("save into an impossible path must fail")
	}
}

func TestPlaysCountAndFollowRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if n, err := s.RecordPlay("old/a.mp3"); err != nil || n != i {
			t.Fatalf("RecordPlay = %d, %v; want %d", n, err, i)
		}
	}
	if err := s.MovePlays("old", "new"); err != nil {
		t.Fatal(err)
	}
	if s.Plays("old/a.mp3") != 0 || s.Plays("new/a.mp3") != 2 {
		t.Fatalf("plays did not follow the rename: %d %d", s.Plays("old/a.mp3"), s.Plays("new/a.mp3"))
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Plays("new/a.mp3") != 2 {
		t.Fatal("plays not persisted")
	}
}
