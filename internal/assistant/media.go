package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// groqTranscriptionURL is the OpenAI-compatible Groq audio endpoint.
const groqTranscriptionURL = "https://api.groq.com/openai/v1/audio/transcriptions"

// whisperModel is the Groq speech-to-text model used for voice messages.
const whisperModel = "whisper-large-v3"

// Transcriber turns a downloaded audio file into text via Groq Whisper.
type Transcriber struct {
	apiKey string
	client *http.Client
}

// NewTranscriber returns a transcriber, or nil when no API key is configured.
func NewTranscriber(apiKey string) *Transcriber {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	return &Transcriber{apiKey: apiKey, client: &http.Client{Timeout: 3 * time.Minute}}
}

// Transcribe uploads the audio file and returns the recognised text.
func (t *Transcriber) Transcribe(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	for field, value := range map[string]string{
		"model":           whisperModel,
		"response_format": "json",
	} {
		if err := mw.WriteField(field, value); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqTranscriptionURL, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq transcription: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("groq transcription: status %d: %s", resp.StatusCode, tg.TruncateRunes(string(body), 400))
	}
	var out struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("groq transcription: decode response: %w", err)
	}
	if out.Error.Message != "" {
		return "", fmt.Errorf("groq transcription: %s", out.Error.Message)
	}
	return strings.TrimSpace(out.Text), nil
}

// unsafeFilenameChars matches everything not allowed in a generated inbox filename.
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// inboxPath builds a unique, safe destination path under the inbox directory.
func inboxPath(inboxDir, name string) string {
	name = unsafeFilenameChars.ReplaceAllString(filepath.Base(name), "_")
	name = strings.Trim(name, "._-")
	if name == "" {
		name = "file"
	}
	name = tg.TruncateRunes(name, 80)
	base := time.Now().Format("20060102-150405") + "-" + name

	path := filepath.Join(inboxDir, base)
	for i := 2; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		ext := filepath.Ext(base)
		path = filepath.Join(inboxDir, strings.TrimSuffix(base, ext)+fmt.Sprintf("-%d", i)+ext)
	}
}

// extForMime maps a Telegram mime type to a file extension when the message
// carries no file name of its own.
func extForMime(mime, fallback string) string {
	switch strings.ToLower(mime) {
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "application/pdf":
		return ".pdf"
	}
	return fallback
}

// kindForName classifies an attachment for the app's message list.
func kindForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic":
		return "image"
	case ".ogg", ".opus", ".mp3", ".m4a", ".wav", ".aac":
		return "audio"
	case ".mp4", ".mov", ".webm":
		return "video"
	default:
		return "file"
	}
}
