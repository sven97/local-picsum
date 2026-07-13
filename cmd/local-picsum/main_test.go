package main

import (
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
