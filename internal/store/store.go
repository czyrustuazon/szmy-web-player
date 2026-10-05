// Package store persists favorites, settings, resume position and the
// password hash in one JSON file, written atomically.
package store

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Settings are the user's playback preferences.
type Settings struct {
	Shuffle     bool    `json:"shuffle"`
	Repeat      string  `json:"repeat"`      // off | all | one
	Volume      float64 `json:"volume"`      // 0..1
	LoopMode    string  `json:"loopMode"`    // forever | count | once
	LoopCount   int     `json:"loopCount"`   // plays of the loop section before fading out
	FadeSeconds int     `json:"fadeSeconds"` // fade-out after the last loop
	VizMode     string  `json:"vizMode"`     // bars | scope | off
	SkipGuard   bool    `json:"skipGuard"`   // require a double press of lock-screen next/previous
}

// DefaultSettings are used until the user changes something.
func DefaultSettings() Settings {
	return Settings{Repeat: "off", Volume: 1, LoopMode: "count", LoopCount: 2, FadeSeconds: 10, VizMode: "bars"}
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Normalize clamps every field into its valid range.
func (s Settings) Normalize() Settings {
	d := DefaultSettings()
	if !oneOf(s.Repeat, "off", "all", "one") {
		s.Repeat = d.Repeat
	}
	if !oneOf(s.LoopMode, "forever", "count", "once") {
		s.LoopMode = d.LoopMode
	}
	if !oneOf(s.VizMode, "bars", "scope", "off") {
		s.VizMode = d.VizMode
	}
	if s.Volume < 0 || math.IsNaN(s.Volume) {
		s.Volume = 0
	}
	if s.Volume > 1 {
		s.Volume = 1
	}
	if s.LoopCount < 1 {
		s.LoopCount = d.LoopCount
	}
	if s.LoopCount > 20 {
		s.LoopCount = 20
	}
	if s.FadeSeconds < 0 {
		s.FadeSeconds = 0
	}
	if s.FadeSeconds > 30 {
		s.FadeSeconds = 30
	}
	return s
}

// Resume is where playback left off.
type Resume struct {
	Path     string  `json:"path"`
	Position float64 `json:"position"`
	Source   string  `json:"source"` // library | favorites
}

// Favorite is a favorited track.
type Favorite struct {
	Path  string `json:"path"`
	Added int64  `json:"added"`
}

type state struct {
	Favorites map[string]int64 `json:"favorites"`
	Settings  Settings         `json:"settings"`
	Resume    Resume           `json:"resume"`
	Plays     map[string]int   `json:"plays,omitempty"`
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
	now  func() time.Time
}

// Open loads path; a missing file starts empty.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	s.st = state{Favorites: map[string]int64{}, Settings: DefaultSettings()}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("%s is corrupt: %w", path, err)
		}
	}
	if s.st.Favorites == nil {
		s.st.Favorites = map[string]int64{}
	}
	if s.st.Plays == nil {
		s.st.Plays = map[string]int{}
	}
	s.st.Settings = s.st.Settings.Normalize()
	return s, nil
}

// RecordPlay counts one finished listen of path and returns the new total.
func (s *Store) RecordPlay(path string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Plays[path]++
	return s.st.Plays[path], s.save()
}

// Plays returns how many times path has been listened to.
func (s *Store) Plays(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Plays[path]
}

// MovePlays re-roots play counts from folder from to folder to. With
// to == "" they are kept, so an undone delete still has its history.
func (s *Store) MovePlays(from, to string) error {
	if to == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	moved := map[string]int{}
	for p, n := range s.st.Plays {
		if strings.HasPrefix(p, from+"/") {
			delete(s.st.Plays, p)
			moved[to+p[len(from):]] = n
		}
	}
	if len(moved) == 0 {
		return nil
	}
	for p, n := range moved {
		s.st.Plays[p] = n
	}
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Favorites lists favorites, newest first.
func (s *Store) Favorites() []Favorite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Favorite, 0, len(s.st.Favorites))
	for p, t := range s.st.Favorites {
		out = append(out, Favorite{Path: p, Added: t})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Added != out[j].Added {
			return out[i].Added > out[j].Added
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// IsFavorite reports whether path is favorited.
func (s *Store) IsFavorite(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.st.Favorites[path]
	return ok
}

// SetFavorite adds or removes a favorite.
func (s *Store) SetFavorite(path string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, had := s.st.Favorites[path]
	switch {
	case on && !had:
		s.st.Favorites[path] = s.now().UnixNano()
	case !on && had:
		delete(s.st.Favorites, path)
	default:
		return nil
	}
	return s.save()
}

// FavoritesUnder lists the favorited paths inside dir (at any depth).
func (s *Store) FavoritesUnder(dir string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.st.Favorites {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out
}

// MoveFavorites re-roots favorites from folder from to folder to, keeping
// their added times. With to == "" they are dropped.
func (s *Store) MoveFavorites(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for p, t := range s.st.Favorites {
		if !strings.HasPrefix(p, from+"/") {
			continue
		}
		delete(s.st.Favorites, p)
		if to != "" {
			s.st.Favorites[to+p[len(from):]] = t
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return s.save()
}

// Remap moves favorites and play counts from the old path of each file to its new one (after a
// folder merge). A favorite lands on the new path unless that path is a favorite already; play
// counts add up.
func (s *Store) Remap(moves map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for from, to := range moves {
		if t, ok := s.st.Favorites[from]; ok {
			delete(s.st.Favorites, from)
			if _, exists := s.st.Favorites[to]; !exists {
				s.st.Favorites[to] = t
			}
			changed = true
		}
		if n, ok := s.st.Plays[from]; ok {
			delete(s.st.Plays, from)
			s.st.Plays[to] += n
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

// Settings returns the current settings.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Settings
}

// SetSettings validates, stores and returns the settings.
func (s *Store) SetSettings(v Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Settings = v.Normalize()
	return s.st.Settings, s.save()
}

// Resume returns the saved playback position.
func (s *Store) Resume() Resume {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Resume
}

// SetResume stores the playback position.
func (s *Store) SetResume(r Resume) error {
	if r.Position < 0 || math.IsNaN(r.Position) {
		r.Position = 0
	}
	if r.Source != "favorites" {
		r.Source = "library"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Resume = r
	return s.save()
}
