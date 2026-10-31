package wsup

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// Display terminal cells rather than UTF-8 bytes so Japanese text aligns with
// the borders. Ambiguous-width characters use the usual one-cell convention.
func cellWidth(text string) int {
	width := 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			r >= 0x2e80 && r <= 0xa4cf && r != 0x303f || r >= 0xac00 && r <= 0xd7a3 ||
			r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe19 || r >= 0xfe30 && r <= 0xfe6f ||
			r >= 0xff01 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 ||
			r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd):
			width += 2
		default:
			width++
		}
	}
	return width
}

// Repository filenames and model-generated summaries must not inject terminal
// control sequences or extra table rows. Escape controls without changing data.
func tableCell(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			escaped := strconv.QuoteRune(r)
			b.WriteString(escaped[1 : len(escaped)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeTable(out io.Writer, rows [][]string) error {
	bold := false
	if f, ok := out.(*os.File); ok {
		info, err := f.Stat()
		_, noColor := os.LookupEnv("NO_COLOR")
		bold = err == nil && info.Mode()&os.ModeCharDevice != 0 && !noColor && os.Getenv("TERM") != "dumb"
	}
	return writeTableStyled(out, rows, bold)
}

func writeTableStyled(out io.Writer, rows [][]string, bold bool) error {
	if len(rows) == 0 {
		return nil
	}
	widths := make([]int, len(rows[0]))
	cells := make([][]string, len(rows))
	for i, row := range rows {
		if len(row) != len(widths) {
			return fmt.Errorf("inconsistent table columns")
		}
		cells[i] = make([]string, len(row))
		for j, cell := range row {
			cells[i][j] = tableCell(cell)
			if w := cellWidth(cells[i][j]); w > widths[j] {
				widths[j] = w
			}
		}
	}
	var b strings.Builder
	for i, row := range cells {
		for j, cell := range row {
			if i == 0 && bold {
				b.WriteString("\x1b[1m")
			}
			b.WriteString(cell)
			if i == 0 && bold {
				b.WriteString("\x1b[22m")
			}
			if j < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[j]-cellWidth(cell)+2))
			}
		}
		b.WriteByte('\n')
		if i == 0 {
			width := 0
			for _, w := range widths {
				width += w
			}
			if len(widths) > 1 {
				width += 2 * (len(widths) - 1)
			}
			b.WriteString(strings.Repeat("─", width))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
