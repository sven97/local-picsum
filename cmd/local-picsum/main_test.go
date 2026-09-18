package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCompactDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{6 * time.Hour, "6h"},
		{90 * time.Minute, "90m"},
		{90 * time.Second, "1m30s"},
	} {
		if got := compactDuration(tc.in); got != tc.want {
			t.Errorf("compactDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAdminAssetsArePublicButPageRequiresAuthentication(t *testing.T) {
	a := &app{}

	assetResponse := httptest.NewRecorder()
	a.admin(assetResponse, httptest.NewRequest(http.MethodGet, "/admin/assets/app.css", nil))
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("public admin asset returned %d, want 200", assetResponse.Code)
	}
	if got := assetResponse.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Fatalf("asset content type = %q", got)
	}

	pageResponse := httptest.NewRecorder()
	a.admin(pageResponse, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if pageResponse.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated admin page returned %d, want 303", pageResponse.Code)
	}
	if got := pageResponse.Header().Get("Location"); got != "/login" {
		t.Fatalf("redirect location = %q, want /login", got)
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

func mustWriteJPEG(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSkipsThumbnailCacheDir(t *testing.T) {
	root := t.TempDir()
	mustWriteJPEG(t, filepath.Join(root, "Album", "photo1.jpg"))
	mustWriteJPEG(t, filepath.Join(root, "Album", "@eaDir", "photo1.jpg", "SYNOPHOTO_THUMB_XL.jpg"))

	a := newTestApp(t)
	a.root = root
	if _, err := a.db.Exec("INSERT INTO folders(path) VALUES('')"); err != nil {
		t.Fatal(err)
	}

	a.refresh()

	var count int
	if err := a.db.QueryRow("SELECT count(*) FROM photos").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 indexed photo (thumbnail cache excluded), got %d", count)
	}
	var path string
	if err := a.db.QueryRow("SELECT path FROM photos").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "Album/photo1.jpg" {
		t.Fatalf("expected only Album/photo1.jpg indexed, got %q", path)
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
		"":                     1, // stray file directly under the library root
		"2022_Photos":          2,
		"2022_Photos/Vacation": 5,
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

func TestApplyOrientation(t *testing.T) {
	// 3x2 source, one distinct color per pixel:
	//   A B C
	//   D E F
	A := color.RGBA{R: 255, A: 255}
	B := color.RGBA{G: 255, A: 255}
	C := color.RGBA{B: 255, A: 255}
	D := color.RGBA{R: 255, G: 255, A: 255}
	E := color.RGBA{G: 255, B: 255, A: 255}
	F := color.RGBA{R: 255, B: 255, A: 255}

	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	src.SetRGBA(0, 0, A)
	src.SetRGBA(1, 0, B)
	src.SetRGBA(2, 0, C)
	src.SetRGBA(0, 1, D)
	src.SetRGBA(1, 1, E)
	src.SetRGBA(2, 1, F)

	cases := []struct {
		o    int
		w, h int
		grid [][]color.RGBA // grid[y][x]
	}{
		{1, 3, 2, [][]color.RGBA{{A, B, C}, {D, E, F}}},
		{2, 3, 2, [][]color.RGBA{{C, B, A}, {F, E, D}}},
		{3, 3, 2, [][]color.RGBA{{F, E, D}, {C, B, A}}},
		{4, 3, 2, [][]color.RGBA{{D, E, F}, {A, B, C}}},
		{5, 2, 3, [][]color.RGBA{{A, D}, {B, E}, {C, F}}},
		{6, 2, 3, [][]color.RGBA{{D, A}, {E, B}, {F, C}}},
		{7, 2, 3, [][]color.RGBA{{F, C}, {E, B}, {D, A}}},
		{8, 2, 3, [][]color.RGBA{{C, F}, {B, E}, {A, D}}},
	}

	for _, tc := range cases {
		got := applyOrientation(src, tc.o)
		if got.Bounds().Dx() != tc.w || got.Bounds().Dy() != tc.h {
			t.Fatalf("o=%d: got bounds %v, want %dx%d", tc.o, got.Bounds(), tc.w, tc.h)
		}
		for y, row := range tc.grid {
			for x, want := range row {
				if px := got.At(x, y); px != want {
					t.Fatalf("o=%d: pixel (%d,%d) = %v, want %v", tc.o, x, y, px, want)
				}
			}
		}
	}
}

func TestApplyOrientationIdentityReturnsSameImage(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for _, o := range []int{0, 1, 9} {
		if got := applyOrientation(src, o); got != image.Image(src) {
			t.Fatalf("o=%d: expected src returned unchanged", o)
		}
	}
}

// exifOrientationSegment builds a minimal JPEG APP1 EXIF segment
// containing only the Orientation tag.
func exifOrientationSegment(o uint16) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(42))
	binary.Write(&tiff, binary.LittleEndian, uint32(8))

	binary.Write(&tiff, binary.LittleEndian, uint16(1)) // 1 IFD entry
	binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	binary.Write(&tiff, binary.LittleEndian, uint16(3)) // type SHORT
	binary.Write(&tiff, binary.LittleEndian, uint32(1)) // count
	binary.Write(&tiff, binary.LittleEndian, uint32(o)) // value (low 2 bytes)
	binary.Write(&tiff, binary.LittleEndian, uint32(0)) // next IFD: none

	var payload bytes.Buffer
	payload.WriteString("Exif\x00\x00")
	payload.Write(tiff.Bytes())

	var seg bytes.Buffer
	seg.WriteByte(0xFF)
	seg.WriteByte(0xE1)
	binary.Write(&seg, binary.BigEndian, uint16(payload.Len()+2))
	seg.Write(payload.Bytes())
	return seg.Bytes()
}

// jpegWithOrientation encodes img as JPEG and, if o != 0, injects an
// EXIF APP1 segment carrying Orientation=o right after the SOI marker.
func jpegWithOrientation(t *testing.T, img image.Image, o uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if e := jpeg.Encode(&buf, img, nil); e != nil {
		t.Fatalf("encode: %v", e)
	}
	raw := buf.Bytes()
	if o == 0 {
		return raw
	}
	seg := exifOrientationSegment(o)
	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...)
	out = append(out, seg...)
	out = append(out, raw[2:]...)
	return out
}

func solidHalves(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 255, A: 255}
	blue := color.RGBA{B: 255, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.SetRGBA(x, y, red)
			} else {
				img.SetRGBA(x, y, blue)
			}
		}
	}
	return img
}

func closeTo(c color.Color, want color.RGBA, tol int) bool {
	r, g, b, _ := c.RGBA()
	wr, wg, wb, _ := want.RGBA()
	diff := func(a, b uint32) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	// RGBA() returns 16-bit-scaled values; scale tol the same way.
	tol16 := tol * 257
	return diff(r, wr) <= tol16 && diff(g, wg) <= tol16 && diff(b, wb) <= tol16
}

func TestJPEGOrientationReadsExifTag(t *testing.T) {
	img := solidHalves(40, 20)
	b := jpegWithOrientation(t, img, 6)
	if o := jpegOrientation(b); o != 6 {
		t.Fatalf("got orientation %d, want 6", o)
	}
}

func TestJPEGOrientationDefaultsWithoutExif(t *testing.T) {
	img := solidHalves(40, 20)
	b := jpegWithOrientation(t, img, 0) // no EXIF segment injected
	if o := jpegOrientation(b); o != 1 {
		t.Fatalf("got orientation %d, want 1", o)
	}
}

func TestDecodeAppliesJPEGOrientation(t *testing.T) {
	img := solidHalves(40, 20) // left half red, right half blue
	b := jpegWithOrientation(t, img, 6)
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if e := os.WriteFile(path, b, 0644); e != nil {
		t.Fatalf("write: %v", e)
	}

	got, e := decode(path)
	if e != nil {
		t.Fatalf("decode: %v", e)
	}
	if got.Bounds().Dx() != 20 || got.Bounds().Dy() != 40 {
		t.Fatalf("got bounds %v, want 20x40 (dimensions should swap on 90deg rotation)", got.Bounds())
	}
	// The vertical red/blue split becomes a horizontal split after a 90deg CW rotation.
	if px := got.At(5, 5); !closeTo(px, color.RGBA{R: 255, A: 255}, 40) {
		t.Fatalf("top region: got %v, want red-ish", px)
	}
	if px := got.At(5, 35); !closeTo(px, color.RGBA{B: 255, A: 255}, 40) {
		t.Fatalf("bottom region: got %v, want blue-ish", px)
	}
}

func TestDecodeLeavesImageUnchangedWithoutEXIF(t *testing.T) {
	img := solidHalves(40, 20)
	b := jpegWithOrientation(t, img, 0) // no EXIF segment injected
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if e := os.WriteFile(path, b, 0644); e != nil {
		t.Fatalf("write: %v", e)
	}

	got, e := decode(path)
	if e != nil {
		t.Fatalf("decode: %v", e)
	}
	if got.Bounds().Dx() != 40 || got.Bounds().Dy() != 20 {
		t.Fatalf("got bounds %v, want unchanged 40x20", got.Bounds())
	}
}
