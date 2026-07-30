package sip

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const realtimeURL = "wss://api.x.ai/v1/realtime"

// SessionConfig configures a Grok Voice Realtime session.
type SessionConfig struct {
	Voice        string
	Instructions string
	Model        string
	AudioRate    int
	OnDownlink   func(pcm []byte)
	OnEvent      func(eventType string)
}

// Session is a live Grok Realtime WebSocket.
type Session struct {
	conn   *websocket.Conn
	cfg    SessionConfig
	mu     sync.Mutex
	closed bool
}

// DialDirect opens a model-based realtime session (phone audio bridged by us).
func DialDirect(ctx context.Context, apiKey string, cfg SessionConfig) (*Session, error) {
	model := cfg.Model
	if model == "" {
		model = "grok-voice-latest"
	}
	url := fmt.Sprintf("%s?model=%s", realtimeURL, model)
	return dial(ctx, apiKey, url, cfg)
}

func dial(ctx context.Context, apiKey, url string, cfg SessionConfig) (*Session, error) {
	if cfg.AudioRate == 0 {
		cfg.AudioRate = 16000
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+apiKey)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, url, hdr)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("ws dial: %w (http %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	s := &Session{conn: conn, cfg: cfg}
	if err := s.configure(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go s.readLoop()
	return s, nil
}

func (s *Session) configure() error {
	update := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"voice":        s.cfg.Voice,
			"instructions": s.cfg.Instructions,
			"turn_detection": map[string]any{
				"type": "server_vad",
			},
			"audio": map[string]any{
				"input": map[string]any{
					"format":    map[string]any{"type": "audio/pcm", "rate": s.cfg.AudioRate},
					"transport": "binary",
				},
				"output": map[string]any{
					"format":    map[string]any{"type": "audio/pcm", "rate": s.cfg.AudioRate},
					"transport": "binary",
				},
			},
		},
	}
	if err := s.conn.WriteJSON(update); err != nil {
		return fmt.Errorf("session.update: %w", err)
	}
	if err := s.conn.WriteJSON(map[string]any{"type": "response.create"}); err != nil {
		return fmt.Errorf("response.create: %w", err)
	}
	return nil
}

// SendPCM uploads uplink audio (binary preferred; JSON fallback).
func (s *Session) SendPCM(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("session closed")
	}
	if err := s.conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		b64 := base64.StdEncoding.EncodeToString(pcm)
		return s.conn.WriteJSON(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": b64,
		})
	}
	return nil
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"))
	_ = s.conn.Close()
}

func (s *Session) readLoop() {
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				log.Printf("sip realtime read: %v", err)
			}
			return
		}
		if mt == websocket.BinaryMessage {
			if s.cfg.OnDownlink != nil && len(data) > 0 {
				s.cfg.OnDownlink(data)
			}
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		t, _ := msg["type"].(string)
		if s.cfg.OnEvent != nil && t != "" {
			s.cfg.OnEvent(t)
		}
		switch t {
		case "response.output_audio.delta", "response.audio.delta":
			if b64, ok := msg["delta"].(string); ok && s.cfg.OnDownlink != nil {
				if raw, err := base64.StdEncoding.DecodeString(b64); err == nil && len(raw) > 0 {
					s.cfg.OnDownlink(raw)
				}
			} else if b64, ok := msg["audio"].(string); ok && s.cfg.OnDownlink != nil {
				if raw, err := base64.StdEncoding.DecodeString(b64); err == nil && len(raw) > 0 {
					s.cfg.OnDownlink(raw)
				}
			}
		case "error":
			log.Printf("sip realtime error: %v", msg)
		}
	}
}
