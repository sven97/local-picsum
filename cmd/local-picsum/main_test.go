package main

import (
	"database/sql"
	"errors"
	"image"
	"image/color"
	"testing"
)

func TestPositive(t *testing.T) {
	for _, v := range []string{"0", "-1", "10001", "x"} {
		if _, err := positive(v); err == nil {
			t.Fatalf("%q should fail", v)
		}
	}
	if n, err := positive("800"); err != nil || n != 800 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestCoverUsesRequestedDimensions(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 400; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	got := cover(src, 80, 80)
	if got.Bounds().Dx() != 80 || got.Bounds().Dy() != 80 {
		t.Fatalf("got %v", got.Bounds())
	}
}

func TestSafePathRejectsEscape(t *testing.T) {
	a := &app{root: "/photos"}
	if _, err := a.safePath("../private"); err == nil {
		t.Fatal("expected path escape to be rejected")
	}
	if p, err := a.safePath("albums/2026"); err != nil || p != "/photos/albums/2026" {
		t.Fatalf("got %q, %v", p, err)
	}
}

func TestBuildTreeMarksSelectedAndDisablesDescendants(t *testing.T) {
	dirs := []string{"2022_Photos", "2022_Photos/Vacation", "2022_Photos/Vacation/Alaska", "2023_Photos"}
	selected := map[string]bool{"2022_Photos": true}
	root := buildTree(dirs, selected, nil)

	if root.Path != "" || root.Name != "/ (library root)" {
		t.Fatalf("unexpected root: %+v", root)
	}
	if root.Disabled {
		t.Fatalf("root must never be disabled")
	}
	if len(root.Children) != 2 {
		t.Fatalf("expected 2 top-level children, got %d", len(root.Children))
	}

	var photos2022, photos2023 *node
	for _, c := range root.Children {
		switch c.Path {
		case "2022_Photos":
			photos2022 = c
		case "2023_Photos":
			photos2023 = c
		}
	}
	if photos2022 == nil || photos2023 == nil {
		t.Fatalf("missing expected top-level nodes: %+v", root.Children)
	}
	if !photos2022.Selected {
		t.Fatalf("2022_Photos should be selected")
	}
	if photos2023.Selected || photos2023.Disabled {
		t.Fatalf("2023_Photos should be untouched: %+v", photos2023)
	}
	if len(photos2022.Children) != 1 || photos2022.Children[0].Path != "2022_Photos/Vacation" {
		t.Fatalf("unexpected children of 2022_Photos: %+v", photos2022.Children)
	}
	vacation := photos2022.Children[0]
	if !vacation.Disabled {
		t.Fatalf("Vacation should be disabled (covered by selected parent)")
	}
	if len(vacation.Children) != 1 || !vacation.Children[0].Disabled {
		t.Fatalf("Alaska should also be disabled (covered by selected grandparent): %+v", vacation.Children)
	}
}

func newTestApp(t *testing.T) *app {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // keep all queries on the same in-memory database
	a := &app{db: db, root: "/photos"}
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAddFolderRejectsWhenAncestorAlreadySelected(t *testing.T) {
	a := newTestApp(t)
	if err := a.addFolder("2022_Photos"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := a.addFolder("2022_Photos/Vacation")
	if !errors.Is(err, errFolderCovered) {
		t.Fatalf("expected errFolderCovered, got %v", err)
	}
	paths, _ := a.folderPaths()
	if len(paths) != 1 || paths[0] != "2022_Photos" {
		t.Fatalf("rejected add must not change folder set, got %v", paths)
	}
}

func TestAddFolderRemovesRedundantDescendants(t *testing.T) {
	a := newTestApp(t)
	if err := a.addFolder("2022_Photos/Vacation"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := a.addFolder("2022_Photos"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	paths, err := a.folderPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "2022_Photos" {
		t.Fatalf("expected only [2022_Photos] after adding ancestor, got %v", paths)
	}
}

func TestIsAncestor(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "x", true},
		{"", "", false},
		{"a", "a/b", true},
		{"a", "a/b/c", true},
		{"a", "ab", false},
		{"a/b", "a", false},
		{"a", "a", false},
	}
	for _, c := range cases {
		if got := isAncestor(c.a, c.b); got != c.want {
			t.Errorf("isAncestor(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestBuildTreeAggregatesCountsUpTheTree(t *testing.T) {
	dirs := []string{"2022_Photos", "2022_Photos/Vacation"}
	counts := map[string]int{
		"":                        1, // stray file directly under the library root
		"2022_Photos":             2,
		"2022_Photos/Vacation":    5,
	}
	root := buildTree(dirs, nil, counts)
	if root.Count != 8 {
		t.Fatalf("expected root count 8 (1+2+5), got %d", root.Count)
	}
	photos2022 := root.Children[0]
	if photos2022.Path != "2022_Photos" || photos2022.Count != 7 {
		t.Fatalf("expected 2022_Photos count 7 (2+5), got %+v", photos2022)
	}
	vacation := photos2022.Children[0]
	if vacation.Path != "2022_Photos/Vacation" || vacation.Count != 5 {
		t.Fatalf("expected Vacation count 5, got %+v", vacation)
	}
}

func TestBuildTreeAutoCreatesMissingAncestors(t *testing.T) {
	root := buildTree([]string{"a/b/c"}, nil, nil)
	if len(root.Children) != 1 || root.Children[0].Path != "a" {
		t.Fatalf("expected auto-created 'a' node, got %+v", root.Children)
	}
	b := root.Children[0].Children
	if len(b) != 1 || b[0].Path != "a/b" {
		t.Fatalf("expected auto-created 'a/b' node, got %+v", b)
	}
	if len(b[0].Children) != 1 || b[0].Children[0].Path != "a/b/c" || b[0].Children[0].Name != "c" {
		t.Fatalf("expected 'a/b/c' leaf node, got %+v", b[0].Children)
	}
}
