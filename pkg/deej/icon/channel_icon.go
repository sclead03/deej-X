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
	IconWidth  = 36
	IconHeight = 36

	wireWidth   = 128
	wirePages   = 6                            // SSD1306 pages 2–7 (blue area, rows 16–63)
	WireBytes   = wireWidth * wirePages        // 768
	iconLeftPad = (wireWidth - IconWidth) / 2  // 46 — default horizontal centering
	iconTopPad  = (wirePages*8 - IconHeight) / 2 // 6 — default vertical centering
)

// Load returns a 768-byte SSD1306 page-order frame for the given process name,
// with the icon centered in the blue area. Equivalent to LoadAt with the
// default centered offset.
func Load(processName, iconDir string) ([]byte, error) {
	return LoadAt(processName, iconDir, iconLeftPad, iconTopPad)
}

// LoadAt is identical to Load but places the icon at an arbitrary (xOffset, yOffset)
// within the 128x48 blue area instead of centering it — used to reposition a
// channel's icon during the screensaver bounce (CMD_REQUEST_ICON_REDRAW). Offsets
// are clamped to keep the icon fully on-screen.
func LoadAt(processName, iconDir string, xOffset, yOffset int) ([]byte, error) {
	mono, err := loadMono(processName, iconDir)
	if err != nil {
		return nil, err
	}

	if xOffset < 0 {
		xOffset = 0
	} else if maxX := wireWidth - IconWidth; xOffset > maxX {
		xOffset = maxX
	}
	if yOffset < 0 {
		yOffset = 0
	} else if maxY := wirePages*8 - IconHeight; yOffset > maxY {
		yOffset = maxY
	}

	return packSSD1306(mono, xOffset, yOffset), nil
}

// loadMono decodes processName's icon PNG and converts it to a IconWidth x
// IconHeight row-major 1-bit mask. processName has ".exe" stripped and is
// matched case-insensitively to a PNG file in iconDir. "deej.unmapped" maps to
// "unmapped.png". Transparent images use alpha as the content mask; opaque
// images are grayscaled and thresholded at 128.
func loadMono(processName, iconDir string) ([]bool, error) {
	base := strings.TrimSuffix(strings.ToLower(processName), ".exe")
	if strings.HasPrefix(base, "deej.") {
		base = strings.TrimPrefix(base, "deej.")
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
		alphaScaled := boxResizeAlpha(nrgba, IconWidth, IconHeight)
		for i, a := range alphaScaled {
			mono[i] = a >= 128
		}
	} else {
		scaled := boxResize(nrgba, IconWidth, IconHeight)
		gray := toGray(scaled)
		applyThreshold(gray, mono)
	}

	return mono, nil
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

// packSSD1306 converts a row-major mono bitmap into a 768-byte SSD1306 page-order
// frame, placing the icon's top-left corner at (leftPad, topPad). Each byte = one
// column of 8 vertical pixels; bit 0 = topmost pixel of that page row.
func packSSD1306(mono []bool, leftPad, topPad int) []byte {
	wire := make([]byte, WireBytes)

	for page := 0; page < wirePages; page++ {
		for col := 0; col < wireWidth; col++ {
			iconCol := col - leftPad
			if iconCol < 0 || iconCol >= IconWidth {
				continue
			}
			var b byte
			for bit := 0; bit < 8; bit++ {
				row := page*8 + bit - topPad
				if row >= 0 && row < IconHeight && mono[row*IconWidth+iconCol] {
					b |= 1 << uint(bit)
				}
			}
			wire[page*wireWidth+col] = b
		}
	}

	return wire
}
