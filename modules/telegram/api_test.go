package telegram

import (
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

func TestNormalizeAndAllow(t *testing.T) {
	def, chats := store.NormalizeTelegramChats(" 100 ", []store.TelegramChat{
		{ID: "100", Label: "me"},
		{ID: "200", Label: "group"},
		{ID: "200", Label: ""},
		{ID: "  ", Label: "x"},
	})
	if def != "100" {
		t.Fatalf("default: %q", def)
	}
	if len(chats) != 2 {
		t.Fatalf("chats: %+v", chats)
	}
	if !store.ChatAllowed(def, chats, "100") || !store.ChatAllowed(def, chats, "200") {
		t.Fatal("expected allowlist hits")
	}
	if store.ChatAllowed(def, chats, "999") {
		t.Fatal("stranger chat should be rejected")
	}
}

func TestFormatChatLabel(t *testing.T) {
	if got := FormatChatLabel(discoveredChat{Title: "Family"}); got != "Family" {
		t.Fatal(got)
	}
	if got := FormatChatLabel(discoveredChat{First: "J", Last: "C", Username: "j"}); got != "J C (@j)" {
		t.Fatal(got)
	}
}
