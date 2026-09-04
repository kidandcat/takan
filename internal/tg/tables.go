package tg

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// tableSepCell is a GFM alignment cell: ---:  ---  :---  :---:
var tableSepCell = regexp.MustCompile(`^:?-{3,}:?$`)

// rewriteTablesForTelegram turns GitHub-flavored markdown tables into aligned
// fenced code blocks. Telegram's Markdown parse mode does not render tables, so
// the raw pipes wrap into unreadable soup on a phone.
func RewriteTablesForTelegram(src string) string {
	if !strings.Contains(src, "|") {
		return src
	}
	lines := strings.Split(src, "\n")
	var out []string
	inFence := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}
		if !inFence && i+1 < len(lines) && isTableRow(line) && isTableSeparator(lines[i+1]) {
			start := i
			i += 2
			for i < len(lines) && isTableRow(lines[i]) {
				i++
			}
			out = append(out, formatTable(lines[start:i])...)
			continue
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

func isTableSeparator(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" {
		return false
	}
	s = strings.Trim(s, "|")
	parts := strings.Split(s, "|")
	cells := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !tableSepCell.MatchString(p) {
			return false
		}
		cells++
	}
	return cells >= 1
}

func isTableRow(line string) bool {
	if isTableSeparator(line) {
		return false
	}
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "```") {
		return false
	}
	return strings.Contains(s, "|")
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "|") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "|") {
		s = s[:len(s)-1]
	}
	raw := strings.Split(s, "|")
	cells := make([]string, len(raw))
	for i, c := range raw {
		cells[i] = collapseCell(strings.TrimSpace(c))
	}
	return cells
}

// collapseCell flattens common markdown emphasis so the monospace table stays
// compact. Telegram will not bold inside a code fence anyway.
func collapseCell(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "~~", "")
	return strings.Join(strings.Fields(s), " ")
}

func formatTable(rawLines []string) []string {
	var rows [][]string
	for i, line := range rawLines {
		if i == 1 && isTableSeparator(line) {
			continue
		}
		rows = append(rows, splitTableRow(line))
	}
	if len(rows) == 0 {
		return rawLines
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return rawLines
	}
	widths := make([]int, cols)
	for _, r := range rows {
		for c := 0; c < cols; c++ {
			cell := tableCell(r, c)
			if n := utf8.RuneCountInString(cell); n > widths[c] {
				widths[c] = n
			}
		}
	}
	var body []string
	for i, r := range rows {
		body = append(body, joinPadded(r, widths))
		if i == 0 {
			seps := make([]string, cols)
			for c := 0; c < cols; c++ {
				n := widths[c]
				if n < 3 {
					n = 3
				}
				seps[c] = strings.Repeat("-", n)
			}
			body = append(body, strings.Join(seps, "  "))
		}
	}
	out := make([]string, 0, len(body)+2)
	out = append(out, "```")
	out = append(out, body...)
	out = append(out, "```")
	return out
}

func tableCell(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func joinPadded(row []string, widths []int) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		cell := tableCell(row, i)
		pad := w - utf8.RuneCountInString(cell)
		if pad < 0 {
			pad = 0
		}
		parts[i] = cell + strings.Repeat(" ", pad)
	}
	return strings.Join(parts, "  ")
}
