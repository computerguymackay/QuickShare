package main

import (
	"io"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// qrModulePixels is the PNG size of one QR module.
const qrModulePixels = 8

// renderQR draws the code with Unicode half blocks, two module rows per
// text line. With color on, it forces white-on-black so it scans on both
// light and dark terminals.
func renderQR(w io.Writer, text string, color bool) error {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return err
	}
	bm := q.Bitmap() // true = dark module; includes the quiet zone
	var b strings.Builder
	for y := 0; y < len(bm); y += 2 {
		if color {
			b.WriteString("\x1b[97;40m")
		}
		for x := range bm[y] {
			top := !bm[y][x]
			bottom := y+1 < len(bm) && !bm[y+1][x]
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		if color {
			b.WriteString("\x1b[0m")
		}
		b.WriteString("\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func writeQRPNG(path, text string) error {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return err
	}
	png, err := q.PNG(-qrModulePixels)
	if err != nil {
		return err
	}
	return os.WriteFile(path, png, 0o644)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
