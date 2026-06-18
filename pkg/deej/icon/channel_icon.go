package icon

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

const (
	IconWidth  = 48
	IconHeight = 48

	wireWidth   = 128
	wirePages   = 6             // SSD1306 pages 2–7 (blue area, rows 16–63)
	WireBytes   = wireWidth * wirePages // 768
	iconLeftPad = (wireWidth - IconWidth) / 2 // 40
)

// Load returns a 768-byte SSD1306 page-order frame for the given process name.
// processName has ".exe" stripped and matched case-insensitively to a PNG file
// in iconDir. "deej.unmapped" maps to "unmapped.png".
// conversion is "dither" (Floyd-Steinberg) or "threshold" — used only for fully
// opaque images. Transparent images use alpha as the content mask instead.
func Load(processName, iconDir, conversion string) ([]byte, error) {
	base := strings.TrimSuffix(strings.ToLower(processName), ".exe")
	if base == "deej.unmapped" {
		base = "unmapped"
	}
	p := filepath.Join(iconDir, base+".png")

	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("icon %s: %w", p, err)
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}

	// Convert to NRGBA for consistent non-premultiplied alpha and color access,
	// regardless of the PNG's internal color model.
	b := src.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), src, b.Min, draw.Src)

	mono := make([]bool, IconWidth*IconHeight)

	if hasTransparency(nrgba) {
		// Use alpha channel as content mask: transparent=off, opaque=on.
		// Apply the same dither/threshold choice as the grayscale path so that
		// anti-aliased edges are smoothed rather than hard-cut.
		alphaScaled := boxResizeAlpha(nrgba, IconWidth, IconHeight)
		switch conversion {
		case "threshold":
			for i, a := range alphaScaled {
				mono[i] = a >= 128
			}
		default:
			applyFloydSteinbergAlpha(alphaScaled, mono)
		}
	} else {
		// Fully opaque image: use grayscale brightness + threshold or dither.
		scaled := boxResize(nrgba, IconWidth, IconHeight)
		gray := toGray(scaled)
		switch conversion {
		case "threshold":
			applyThreshold(gray, mono)
		default:
			applyFloydSteinberg(gray, mono)
		}
	}

	return packSSD1306(mono), nil
}

// hasTransparency returns true if any pixel in img has alpha < 255.
func hasTransparency(img *image.NRGBA) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.NRGBAAt(x, y).A < 255 {
				return true
			}
		}
	}
	return false
}

// boxResizeAlpha downscales src to dstW×dstH using box averaging on the alpha
// channel only. Returns one alpha value per output pixel in row-major order.
func boxResizeAlpha(src *image.NRGBA, dstW, dstH int) []uint8 {
	out := make([]uint8, dstW*dstH)
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()

	for dy := 0; dy < dstH; dy++ {
		for dx := 0; dx < dstW; dx++ {
			sx0 := sb.Min.X + dx*sw/dstW
			sy0 := sb.Min.Y + dy*sh/dstH
			sx1 := sb.Min.X + (dx+1)*sw/dstW
			sy1 := sb.Min.Y + (dy+1)*sh/dstH
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sy1 <= sy0 {
				sy1 = sy0 + 1
			}

			var aSum uint64
			n := uint64((sx1 - sx0) * (sy1 - sy0))
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					aSum += uint64(src.NRGBAAt(sx, sy).A)
				}
			}
			out[dy*dstW+dx] = uint8(aSum / n)
		}
	}
	return out
}

// boxResize scales src to dstW×dstH using box averaging on RGB.
// Used only for fully opaque images (no transparency).
func boxResize(src image.Image, dstW, dstH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	sb := src.Bounds()
	sw := sb.Max.X - sb.Min.X
	sh := sb.Max.Y - sb.Min.Y

	for dy := 0; dy < dstH; dy++ {
		for dx := 0; dx < dstW; dx++ {
			sx0 := sb.Min.X + dx*sw/dstW
			sy0 := sb.Min.Y + dy*sh/dstH
			sx1 := sb.Min.X + (dx+1)*sw/dstW
			sy1 := sb.Min.Y + (dy+1)*sh/dstH
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sy1 <= sy0 {
				sy1 = sy0 + 1
			}

			var rSum, gSum, bSum uint64
			n := uint64((sx1 - sx0) * (sy1 - sy0))

			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, b, _ := src.At(sx, sy).RGBA()
					rSum += uint64(r >> 8)
					gSum += uint64(g >> 8)
					bSum += uint64(b >> 8)
				}
			}

			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(rSum / n),
				G: uint8(gSum / n),
				B: uint8(bSum / n),
				A: 255,
			})
		}
	}
	return dst
}

func toGray(src *image.RGBA) *image.Gray {
	b := src.Bounds()
	g := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g.Set(x, y, color.GrayModel.Convert(src.At(x, y)))
		}
	}
	return g
}

func applyThreshold(g *image.Gray, mono []bool) {
	for y := 0; y < IconHeight; y++ {
		for x := 0; x < IconWidth; x++ {
			mono[y*IconWidth+x] = g.GrayAt(x, y).Y >= 128
		}
	}
}

func applyFloydSteinberg(g *image.Gray, mono []bool) {
	buf := make([]float32, IconWidth*IconHeight)
	for y := 0; y < IconHeight; y++ {
		for x := 0; x < IconWidth; x++ {
			buf[y*IconWidth+x] = float32(g.GrayAt(x, y).Y)
		}
	}

	for y := 0; y < IconHeight; y++ {
		for x := 0; x < IconWidth; x++ {
			old := buf[y*IconWidth+x]
			var q float32
			if old >= 128 {
				q = 255
				mono[y*IconWidth+x] = true
			}
			e := old - q

			if x+1 < IconWidth {
				buf[y*IconWidth+x+1] += e * 7 / 16
			}
			if y+1 < IconHeight {
				if x > 0 {
					buf[(y+1)*IconWidth+x-1] += e * 3 / 16
				}
				buf[(y+1)*IconWidth+x] += e * 5 / 16
				if x+1 < IconWidth {
					buf[(y+1)*IconWidth+x+1] += e * 1 / 16
				}
			}
		}
	}
}

func applyFloydSteinbergAlpha(alpha []uint8, mono []bool) {
	buf := make([]float32, IconWidth*IconHeight)
	for i, a := range alpha {
		buf[i] = float32(a)
	}
	for y := 0; y < IconHeight; y++ {
		for x := 0; x < IconWidth; x++ {
			old := buf[y*IconWidth+x]
			var q float32
			if old >= 128 {
				q = 255
				mono[y*IconWidth+x] = true
			}
			e := old - q
			if x+1 < IconWidth {
				buf[y*IconWidth+x+1] += e * 7 / 16
			}
			if y+1 < IconHeight {
				if x > 0 {
					buf[(y+1)*IconWidth+x-1] += e * 3 / 16
				}
				buf[(y+1)*IconWidth+x] += e * 5 / 16
				if x+1 < IconWidth {
					buf[(y+1)*IconWidth+x+1] += e * 1 / 16
				}
			}
		}
	}
}

// packSSD1306 converts a row-major mono bitmap into a 768-byte SSD1306 page-order frame.
// The 48×48 icon is centered horizontally with iconLeftPad (40) zero-padded columns each side.
// Each byte = one column of 8 vertical pixels; bit 0 = topmost pixel of that page row.
func packSSD1306(mono []bool) []byte {
	wire := make([]byte, WireBytes)

	for page := 0; page < wirePages; page++ {
		for col := 0; col < wireWidth; col++ {
			iconCol := col - iconLeftPad
			if iconCol < 0 || iconCol >= IconWidth {
				continue
			}
			var b byte
			for bit := 0; bit < 8; bit++ {
				row := page*8 + bit
				if row < IconHeight && mono[row*IconWidth+iconCol] {
					b |= 1 << uint(bit)
				}
			}
			wire[page*wireWidth+col] = b
		}
	}

	return wire
}
