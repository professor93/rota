package vtscan

// The colours the holder reports: a dark terminal, light grey on black, with
// xterm's default palette.
var (
	foreground = [3]byte{0xd0, 0xd0, 0xd0}
	background = [3]byte{0x00, 0x00, 0x00}
)

// xterm16 is xterm's default for the first sixteen palette entries.
var xterm16 = [16][3]byte{
	{0x00, 0x00, 0x00}, {0xcd, 0x00, 0x00}, {0x00, 0xcd, 0x00}, {0xcd, 0xcd, 0x00},
	{0x00, 0x00, 0xee}, {0xcd, 0x00, 0xcd}, {0x00, 0xcd, 0xcd}, {0xe5, 0xe5, 0xe5},
	{0x7f, 0x7f, 0x7f}, {0xff, 0x00, 0x00}, {0x00, 0xff, 0x00}, {0xff, 0xff, 0x00},
	{0x5c, 0x5c, 0xff}, {0xff, 0x00, 0xff}, {0x00, 0xff, 0xff}, {0xff, 0xff, 0xff},
}

// xtermColor is xterm's default for palette entry n, 0 to 255: sixteen named
// colours, a 6×6×6 cube and a ramp of 24 greys.
func xtermColor(n int) [3]byte {
	switch {
	case n < 16:
		return xterm16[n]
	case n < 232:
		n -= 16
		return [3]byte{cube(n / 36), cube(n / 6 % 6), cube(n % 6)}
	}
	v := byte(8 + 10*(n-232))
	return [3]byte{v, v, v}
}

// cube is one channel of the colour cube: 0, then 95 to 255 in steps of 40.
func cube(i int) byte {
	if i == 0 {
		return 0
	}
	return byte(55 + 40*i)
}

// dynamicColor is OSC 10's foreground or OSC 11's background.
func dynamicColor(code int) [3]byte {
	if code == 10 {
		return foreground
	}
	return background
}
