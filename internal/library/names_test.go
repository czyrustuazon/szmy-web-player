package library

import (
	"path/filepath"
	"testing"
)

func TestFixNamesRenamesEscapedNamesButNotHiddenOnes(t *testing.T) {
	root := t.TempDir()
	l, err := New(root, "", false)
	if err != nil {
		t.Fatal(err)
	}
	w := func(rel string) { mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), mp3) }
	w("#U30a2/#U30aa.mp3")
	w(".uploads/#U30aa.mp3")

	moves, err := l.FixNames(true)
	if err != nil || len(moves) != 1 || moves["#U30a2/#U30aa.mp3"] != "ア/オ.mp3" {
		t.Fatalf("plan: %v %v", moves, err)
	}
	if got := tree(t, root); got != "#U30a2/ #U30a2/#U30aa.mp3 .uploads/ .uploads/#U30aa.mp3" {
		t.Errorf("a plan renames nothing: %s", got)
	}
	if moves, err = l.FixNames(false); err != nil || moves["#U30a2/#U30aa.mp3"] != "ア/オ.mp3" {
		t.Fatalf("fix: %v %v", moves, err)
	}
	if got := tree(t, root); got != ".uploads/ .uploads/#U30aa.mp3 ア/ ア/オ.mp3" {
		t.Errorf("tree: %s", got)
	}

	ro, _ := New(root, "", true)
	if _, err := ro.FixNames(false); err != ErrReadOnly {
		t.Errorf("read-only: %v", err)
	}
	if _, err := ro.FixNames(true); err != nil {
		t.Errorf("a read-only library can still be checked: %v", err)
	}
}
