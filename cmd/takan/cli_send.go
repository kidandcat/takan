package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/kidandcat/takan/internal/assistant"
)

// sendUsage documents the atlas-send helper the CLI agent uses to reach Jairo.
const sendUsage = `atlas-send — push a message or a file to Jairo's Telegram.

Usage:
  atlas-send "some text"
  atlas-send --file /path/to/file [caption]
  atlas-send --chat <chat id> "some text"

Files ending in .jpg/.jpeg/.png/.webp are sent as photos, anything else as a
document. No credentials are needed: the request goes to the hub on
$ATLAS_API (default: the loopback API), which owns the bot token.
`

// runSend implements the atlas-send helper binary.
func runSend(args []string) {
	log.SetFlags(0)
	log.SetPrefix("atlas-send: ")

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(sendUsage)
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}

	// An explicit --chat overrides the owner's own chat.
	var chatID int64
	if args[0] == "--chat" || args[0] == "-c" {
		if len(args) < 3 {
			log.Fatal("--chat requires a chat id and something to send")
		}
		parsed, err := assistant.ParseInt64(args[1])
		if err != nil {
			log.Fatalf("--chat: %v", err)
		}
		chatID = parsed
		args = args[2:]
	}

	if args[0] == "--file" || args[0] == "-f" {
		if len(args) < 2 {
			log.Fatal("--file requires a path")
		}
		path := args[1]
		caption := strings.Join(args[2:], " ")
		if _, err := os.Stat(path); err != nil {
			log.Fatalf("cannot read %s: %v", path, err)
		}
		if err := apiRequest(http.MethodPost, "/internal/send",
			map[string]any{"text": caption, "file": path, "chat_id": chatID}, nil); err != nil {
			log.Fatalf("failed to send file: %v", err)
		}
		fmt.Println("sent", path)
		return
	}

	text := strings.Join(args, " ")
	if strings.TrimSpace(text) == "" {
		log.Fatal("nothing to send")
	}
	if err := apiRequest(http.MethodPost, "/internal/send",
		map[string]any{"text": text, "chat_id": chatID}, nil); err != nil {
		log.Fatalf("failed to send message: %v", err)
	}
	fmt.Println("sent")
}
