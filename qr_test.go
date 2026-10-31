package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

// checkQRPNG verifies that the PNG at path is exactly the QR code for
// text, module by module.
func checkQRPNG(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a valid PNG: %v", err)
	}
	q, _ := qrcode.New(text, qrcode.Medium)
	bm := q.Bitmap()
	if w := img.Bounds().Dx(); w != len(bm)*qrModulePixels {
		t.Fatalf("PNG width %d, want %d", w, len(bm)*qrModulePixels)
	}
	for y := range bm {
		for x := range bm[y] {
			r, _, _, _ := img.At(x*qrModulePixels+qrModulePixels/2, y*qrModulePixels+qrModulePixels/2).RGBA()
			if dark := r < 0x8000; dark != bm[y][x] {
				t.Fatalf("module (%d,%d) does not match the link's QR code", x, y)
			}
		}
	}
}

func TestQRPNGMatchesLink(t *testing.T) {
	link := "http://203.0.113.5:8443/abcdefghijklmnopqrstuvwx/file.zip"
	path := filepath.Join(t.TempDir(), "qr.png")
	if err := writeQRPNG(path, link); err != nil {
		t.Fatal(err)
	}
	checkQRPNG(t, path, link)
}

func TestQRTerminalRender(t *testing.T) {
	link := "http://203.0.113.5:8443/abcdefghijklmnopqrstuvwx/file.zip"
	var buf bytes.Buffer
	if err := renderQR(&buf, link, false); err != nil {
		t.Fatal(err)
	}
	q, _ := qrcode.New(link, qrcode.Medium)
	size := len(q.Bitmap())
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != (size+1)/2 {
		t.Fatalf("%d lines, want %d", len(lines), (size+1)/2)
	}
	for i, l := range lines {
		if n := len([]rune(l)); n != size {
			t.Fatalf("line %d has %d columns, want %d", i, n, size)
		}
	}
	// The quiet zone is light, so the first line is solid blocks.
	if strings.Trim(lines[0], "█") != "" {
		t.Fatal("first line should be quiet zone")
	}
	buf.Reset()
	renderQR(&buf, link, true)
	if !strings.Contains(buf.String(), "\x1b[97;40m") {
		t.Fatal("color mode should set explicit colors")
	}
}
