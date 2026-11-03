package wsup

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// Display terminal cells rather than UTF-8 bytes so Japanese columns align.
// Ambiguous-width characters use the usual one-cell convention.
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

func clipCell(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if cellWidth(text) <= width {
		return text
	}
	var b strings.Builder
	used := 0
	for _, r := range text {
		w := cellWidth(string(r))
		if used+w > width-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…"
}

func writeTable(out io.Writer, rows [][]string) error {
	bold, columns := false, 0
	if f, ok := out.(*os.File); ok {
		info, err := f.Stat()
		_, noColor := os.LookupEnv("NO_COLOR")
		bold = err == nil && info.Mode()&os.ModeCharDevice != 0 && !noColor && os.Getenv("TERM") != "dumb"
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 {
			columns = width
		}
	}
	return writeTableStyled(out, rows, bold, columns)
}

func writeTableStyled(out io.Writer, rows [][]string, bold bool, columns int) error {
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
	gap := 2
	if columns > 0 && len(widths) > 1 {
		gap = min(gap, max(0, (columns-len(widths))/(len(widths)-1)))
	}
	total := gap * max(0, len(widths)-1)
	minimums := make([]int, len(widths))
	for j, width := range widths {
		total += width
		minimums[j] = cellWidth(cells[0][j])
	}
	if columns > 0 {
		// Shrink the widest columns first; keep complete headings when possible.
		for _, floors := range [][]int{minimums, make([]int, len(widths))} {
			for total > columns {
				widest := -1
				for j, width := range widths {
					if width > floors[j] && (widest < 0 || width > widths[widest]) {
						widest = j
					}
				}
				if widest < 0 {
					break
				}
				widths[widest]--
				total--
			}
		}
	}
	lastColumn := len(widths) - 1
	for lastColumn >= 0 && widths[lastColumn] == 0 {
		lastColumn--
	}
	var b strings.Builder
	for i, row := range cells {
		for j, cell := range row {
			if widths[j] == 0 {
				continue
			}
			cell = clipCell(cell, widths[j])
			if i == 0 && bold {
				b.WriteString("\x1b[1m")
			}
			b.WriteString(cell)
			if i == 0 && bold {
				b.WriteString("\x1b[22m")
			}
			if j < lastColumn {
				b.WriteString(strings.Repeat(" ", widths[j]-cellWidth(cell)+gap))
			}
		}
		b.WriteByte('\n')
		if i == 0 {
			b.WriteString(strings.Repeat("─", total))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
