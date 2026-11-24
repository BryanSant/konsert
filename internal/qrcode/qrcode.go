// Package qrcode renders QR codes for sign-in links: compact half-block text for
// terminals and an SVG path for the web app.
package qrcode

import (
	"fmt"
	"io"
	"strings"

	"rsc.io/qr"
)

// quiet is the margin, in modules, around the code. The spec asks for 4; 2 scans
// reliably on screens and keeps the terminal rendering small.
const quiet = 2

// SVG is a QR code as an SVG path over a Size×Size viewBox (margin included).
type SVG struct {
	Size int    `json:"size"`
	Path string `json:"path"`
}

// EncodeSVG encodes text as an SVG path of its dark modules.
func EncodeSVG(text string) (SVG, error) {
	c, err := qr.Encode(text, qr.L)
	if err != nil {
		return SVG{}, err
	}
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return SVG{Size: c.Size + 2*quiet, Path: b.String()}, nil
}

// WriteTerminal writes text's QR code to w using half-block characters (two module
// rows per line). With color it pins black-on-white with ANSI codes so the code scans
// on light and dark terminals alike; without, it assumes light text on a dark
// background (light modules are drawn).
func WriteTerminal(w io.Writer, text string, color bool) error {
	c, err := qr.Encode(text, qr.L)
	if err != nil {
		return err
	}
	n := c.Size + 2*quiet
	dark := func(x, y int) bool { return c.Black(x-quiet, y-quiet) }
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		if color {
			b.WriteString("\x1b[30;107m") // black on bright white
		}
		for x := 0; x < n; x++ {
			top, bottom := dark(x, y), y+1 < n && dark(x, y+1)
			if y+1 >= n {
				bottom = false
			}
			if !color {
				// Draw the light modules in the (light) foreground color.
				top, bottom = !top, !bottom
				if y+1 >= n {
					bottom = false
				}
			}
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		if color {
			b.WriteString("\x1b[0m")
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}
