package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"github.com/spf13/viper"
)

const (
	cmdPrefix    = byte(0x00)
	cmdQuery     = byte(0x01)
	cmdSetChIcon = byte(0x03)

	numChannels = 5
	wireWidth   = 128
	wirePages   = 6
	wireBytes   = wireWidth * wirePages // 768

	leftBarCols  = 4   // solid datum bar at left edge:  cols 0–3
	rightBarStart = 124 // solid datum bar at right edge: cols 124–127
	tickStep     = 8   // measurement lines at cols 8, 16, 24, … labeled 1, 2, 3, …
	maxLabel     = 9   // last labeled tick; covers gaps up to 72 px
)

// 5×7 pixel font for digits 0–9.
// Each [7]uint8 is a row bitmask: bit 4 = leftmost column, bit 0 = rightmost column.
var digitGlyphs = [10][7]uint8{
	{0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110}, // 0
	{0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110}, // 1
	{0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111}, // 2
	{0b01110, 0b10001, 0b00001, 0b01110, 0b00001, 0b10001, 0b01110}, // 3
	{0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010}, // 4
	{0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110}, // 5
	{0b01110, 0b10000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110}, // 6
	{0b11111, 0b00001, 0b00010, 0b00100, 0b00100, 0b00100, 0b00100}, // 7
	{0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110}, // 8
	{0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b10001, 0b01110}, // 9
}

func main() {
	port := flag.String("port", "", "override COM port from config.yaml")
	baud := flag.Int("baud", 0, "override baud rate from config.yaml")
	flag.Parse()

	comPort, baudRate := readConfig()
	if *port != "" {
		comPort = *port
	}
	if *baud != 0 {
		baudRate = *baud
	}

	fmt.Printf("display_spacing — SERENITY OLED gap measurement\n\n")
	fmt.Printf("Opening %s at %d baud...\n", comPort, baudRate)

	opts := serial.OpenOptions{
		PortName:        comPort,
		BaudRate:        uint(baudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: 0,
	}

	conn, err := serial.Open(opts)
	if err != nil {
		log.Fatalf("Failed to open serial port: %v\n(Is deej-x.exe still running? Close it first.)", err)
	}
	defer conn.Close()

	time.Sleep(500 * time.Millisecond)
	if err := sendFrame(conn, cmdQuery, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: CMD_QUERY failed: %v\n", err)
	}
	time.Sleep(300 * time.Millisecond)

	bitmap := makeSpacingBitmap()

	fmt.Printf("Sending pattern to %d channel OLEDs...\n", numChannels)
	for ch := byte(0); ch < numChannels; ch++ {
		payload := make([]byte, 1+len(bitmap))
		payload[0] = ch
		copy(payload[1:], bitmap)
		if err := sendFrame(conn, cmdSetChIcon, payload); err != nil {
			fmt.Fprintf(os.Stderr, "  Channel %d: send failed: %v\n", ch, err)
		} else {
			fmt.Printf("  Channel %d: sent\n", ch)
		}
		time.Sleep(20 * time.Millisecond)
	}

	fmt.Printf("\nEach OLED shows: solid left bar | labeled lines 1–%d (every 8 px) | solid right bar\n", maxLabel)
	fmt.Printf("Gap in pixels = label number × 8\n")
}

// makeSpacingBitmap builds the 768-byte SSD1306 frame:
//
//   - Solid 4 px datum bar at the left edge  (cols 0–3, full height)
//   - Full-height tick lines at cols 8, 16, 24 … with labels 1, 2, 3 … at the top
//   - Solid 4 px datum bar at the right edge (cols 124–127, full height)
//
// To measure a gap: look at the physical space between OLED N's right bar and
// OLED N+1's left bar. Find the labeled line whose distance from OLED N+1's
// left bar matches the gap width — that label × 8 is the gap in pixels.
func makeSpacingBitmap() []byte {
	wire := make([]byte, wireBytes)

	// Left datum bar: cols 0–3, all rows.
	for col := 0; col < leftBarCols; col++ {
		for page := 0; page < wirePages; page++ {
			wire[page*wireWidth+col] = 0xFF
		}
	}

	// Right datum bar: cols 124–127, all rows.
	for col := rightBarStart; col < wireWidth; col++ {
		for page := 0; page < wirePages; page++ {
			wire[page*wireWidth+col] = 0xFF
		}
	}

	// Measurement lines: full-height tick + label at top.
	for label := 1; label <= maxLabel; label++ {
		col := label * tickStep
		drawFullTick(wire, col)
		drawLabel(wire, label, col)
	}

	return wire
}

// drawFullTick lights all 48 rows of the given column.
func drawFullTick(wire []byte, col int) {
	for page := 0; page < wirePages; page++ {
		wire[page*wireWidth+col] = 0xFF
	}
}

// drawLabel renders digit n centered above col at the very top of the blue
// area (rows 1–7), clearing a background patch so it reads cleanly against
// the tick line behind it.
func drawLabel(wire []byte, n, col int) {
	// Clear rows 0–8 around the glyph (5 px wide + 1 px border each side = 7 cols,
	// 7 px tall glyph + 1 px top margin + 1 px gap below = 9 rows).
	for row := 0; row <= 8; row++ {
		for x := col - 3; x <= col+3; x++ {
			drawPixel(wire, x, row, false)
		}
	}
	// Draw the 5×7 glyph starting at row 1, centered on col.
	drawGlyph(wire, n, col, 1)
}

// drawGlyph renders a single 5×7 digit centered horizontally on centerX,
// with the top of the glyph at topY.
func drawGlyph(wire []byte, digit, centerX, topY int) {
	startX := centerX - 2 // center a 5 px wide glyph
	g := digitGlyphs[digit]
	for row := 0; row < 7; row++ {
		for col := 0; col < 5; col++ {
			lit := (g[row]>>uint(4-col))&1 == 1
			drawPixel(wire, startX+col, topY+row, lit)
		}
	}
}

// drawPixel sets or clears one pixel in the SSD1306 page-order wire frame.
func drawPixel(wire []byte, x, y int, lit bool) {
	if x < 0 || x >= wireWidth || y < 0 || y >= wirePages*8 {
		return
	}
	page := y / 8
	bit := uint(y % 8)
	idx := page*wireWidth + x
	if lit {
		wire[idx] |= 1 << bit
	} else {
		wire[idx] &^= 1 << bit
	}
}

func sendFrame(w io.Writer, cmdID byte, payload []byte) error {
	frame := make([]byte, 4+len(payload))
	frame[0] = cmdPrefix
	frame[1] = cmdID
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(payload)))
	copy(frame[4:], payload)
	_, err := w.Write(frame)
	return err
}

func readConfig() (comPort string, baudRate int) {
	v := viper.New()
	v.SetConfigName("spacing_config")
	v.SetConfigType("yaml")
	// Always read from the directory containing the exe so the tool works
	// regardless of where it is launched from.
	if exePath, err := os.Executable(); err == nil {
		v.AddConfigPath(filepath.Dir(exePath))
	}
	v.SetDefault("com_port", "COM4")
	v.SetDefault("baud_rate", 115200)
	if err := v.ReadInConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read spacing_config.yaml (%v), using defaults COM4/115200\n", err)
	}
	return v.GetString("com_port"), v.GetInt("baud_rate")
}
