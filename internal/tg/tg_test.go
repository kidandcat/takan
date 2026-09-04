package tg

import (
	"strings"
	"testing"
)

func TestSplitMessageKeepsChunksWithinLimit(t *testing.T) {
	long := strings.Repeat("word ", 3000)
	chunks := SplitMessage(long, MaxMessageRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected the text to be split, got %d chunk(s)", len(chunks))
	}
	for i, chunk := range chunks {
		if n := len([]rune(chunk)); n > MaxMessageRunes {
			t.Fatalf("chunk %d is %d runes, over the %d limit", i, n, MaxMessageRunes)
		}
	}
	if joined := strings.ReplaceAll(strings.Join(chunks, " "), "  ", " "); !strings.HasPrefix(joined, "word word") {
		t.Fatalf("content was mangled: %q", TruncateRunes(joined, 40))
	}
}

func TestSplitMessageShortTextIsUntouched(t *testing.T) {
	chunks := SplitMessage("hello", MaxMessageRunes)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("expected a single untouched chunk, got %#v", chunks)
	}
}

func TestSplitMessagePrefersNewlineBoundaries(t *testing.T) {
	text := strings.Repeat("a", 4000) + "\n" + strings.Repeat("b", 500)
	chunks := SplitMessage(text, MaxMessageRunes)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if strings.Contains(chunks[0], "b") {
		t.Fatal("expected the split to happen at the newline")
	}
}

func TestTruncateRunesCountsRunesNotBytes(t *testing.T) {
	if got := TruncateRunes("áéíóú", 3); got != "áéí" {
		t.Fatalf("got %q, want %q", got, "áéí")
	}
}

func TestNormalizeChatType(t *testing.T) {
	for in, want := range map[string]string{
		"private": ChatPrivate, "group": ChatGroup,
		"supergroup": ChatGroup, "channel": ChatGroup, "": ChatPrivate,
	} {
		if got := NormalizeChatType(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMessageLabels(t *testing.T) {
	group := &Message{
		Chat: Chat{ID: -100, Type: "supergroup", Title: "Casa"},
		From: &User{ID: 9, FirstName: "Olga", Username: "olga"},
	}
	if !group.IsGroup() {
		t.Fatal("a supergroup is a group")
	}
	if group.ChatLabel() != "Casa" || group.SenderLabel() != "Olga" {
		t.Fatalf("labels: %q / %q", group.ChatLabel(), group.SenderLabel())
	}

	anon := &Message{Chat: Chat{ID: 7, Type: "private"}}
	if anon.SenderLabel() != "alguien" {
		t.Fatalf("a message with no sender: %q", anon.SenderLabel())
	}
	if anon.ChatLabel() != "chat 7" {
		t.Fatalf("fallback chat label: %q", anon.ChatLabel())
	}
}

func TestFormatChatLabel(t *testing.T) {
	cases := map[string]DiscoveredChat{
		"Casa":            {ID: "-100", Title: "Casa"},
		"Jairo (@kidand)": {ID: "1", First: "Jairo", Username: "kidand"},
		"@solo":           {ID: "2", Username: "solo"},
		"private 3":       {ID: "3", Type: "private"},
		"4":               {ID: "4"},
	}
	for want, in := range cases {
		if got := FormatChatLabel(in); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestClientWithoutTokenFailsClosed(t *testing.T) {
	// A missing credential must produce a clear error rather than a request to
	// https://api.telegram.org/bot/getMe.
	if _, err := New("").GetMe(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "bot token is not configured") {
		t.Fatalf("expected a clear no-token error, got %v", err)
	}
}
