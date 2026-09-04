package tg

import "strings"

// TruncateRunes shortens s to at most max runes. It counts runes, not bytes, so
// an accented reply is never cut mid-character.
func TruncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// SplitMessage breaks text into chunks of at most maxLen runes, preferring
// paragraph, line and word boundaries so replies stay readable.
func SplitMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		return []string{text}
	}
	var chunks []string
	remaining := []rune(text)

	for len(remaining) > maxLen {
		cut := boundary(remaining, maxLen)
		chunks = append(chunks, strings.TrimRight(string(remaining[:cut]), " \n"))
		remaining = []rune(strings.TrimLeft(string(remaining[cut:]), "\n"))
	}
	if len(remaining) > 0 {
		chunks = append(chunks, string(remaining))
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

// boundary finds the nicest split point at or before maxLen.
func boundary(runes []rune, maxLen int) int {
	window := string(runes[:maxLen])
	for _, sep := range []string{"\n\n", "\n", " "} {
		if idx := strings.LastIndex(window, sep); idx > maxLen/2 {
			return len([]rune(window[:idx])) + len([]rune(sep))
		}
	}
	return maxLen
}
