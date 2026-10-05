package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlayedCountsListens(t *testing.T) {
	e := newEnv(t, false, false)
	var out struct {
		Plays int `json:"plays"`
	}
	decode(t, e.do("POST", "/api/played", map[string]string{"path": "a.mp3"}, nil), &out)
	decode(t, e.do("POST", "/api/played", map[string]string{"path": "a.mp3"}, nil), &out)
	if out.Plays != 2 || e.srv.Store.Plays("a.mp3") != 2 {
		t.Fatalf("plays: %+v", out)
	}
	wantStatus(t, e.do("POST", "/api/played", map[string]string{"path": "missing.mp3"}, nil), 404)
	wantStatus(t, e.do("POST", "/api/played", map[string]string{"path": "notes.txt"}, nil), 415)
	wantStatus(t, e.do("POST", "/api/played", "{", nil), 400)
	os.Mkdir(filepath.Join(e.data, "state.json.tmp"), 0o755) // saving now fails
	wantStatus(t, e.do("POST", "/api/played", map[string]string{"path": "a.mp3"}, nil), 500)
}

func TestFolderBookkeepingFailuresAreLoggedNotFatal(t *testing.T) {
	e := newEnv(t, false, false)
	for _, d := range []string{"gone", "moved", "back"} {
		write(t, filepath.Join(e.root, d, "s.mp3"), mp3With(d, nil))
		wantStatus(t, e.do("POST", "/api/favorite", map[string]any{"path": d + "/s.mp3", "on": true}, nil), 200)
		wantStatus(t, e.do("POST", "/api/played", map[string]string{"path": d + "/s.mp3"}, nil), 200)
	}
	// Delete a folder and keep its undo token for later.
	var del map[string]any
	decode(t, e.do("DELETE", "/api/track?p=back", nil, nil), &del)

	os.Mkdir(filepath.Join(e.data, "state.json.tmp"), 0o755) // from here on the state cannot be saved
	wantStatus(t, e.do("DELETE", "/api/track?p=gone", nil, nil), 200)
	wantStatus(t, e.do("POST", "/api/rename", map[string]string{"path": "moved", "name": "renamed"}, nil), 200)
	wantStatus(t, e.do("POST", "/api/undo", map[string]string{"token": del["token"].(string)}, nil), 200)

	lines, _ := e.logs.Recent(50)
	all := strings.Join(lines, "\n")
	for _, site := range []string{"site=delete", "site=rename", "site=undo"} {
		if !strings.Contains(all, site) {
			t.Errorf("%s should be logged:\n%s", site, all)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "renamed", "s.mp3")); err != nil {
		t.Error("the folder was renamed anyway")
	}
}
