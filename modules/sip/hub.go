package sip

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kidandcat/takan/internal/store"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Authenticator validates device token → device row.
type Authenticator func(ctx context.Context, token string) (*store.SIPDevice, error)

// Touch updates last_seen.
type Touch func(ctx context.Context, deviceID string)

// Hub tracks connected Android SIP gateways (outbound WSS only).
type Hub struct {
	Auth  Authenticator
	Touch Touch

	// SessionLoader provides per-user bridge settings + decrypted API key.
	SessionLoader func(ctx context.Context, userID string) (BridgeConfig, error)

	mu      sync.RWMutex
	clients map[string]*Client // device id → client

	calls *CallManager
}

// BridgeConfig is runtime config for a user's Grok bridge.
type BridgeConfig struct {
	APIKey       string
	Voice        string
	Instructions string
	AutoAnswer   bool
	AudioRate    int
	BridgeMode   string
}

// Client is one connected phone gateway.
type Client struct {
	Device store.SIPDevice
	conn   *websocket.Conn
	send   chan []byte
	hub    *Hub
}

// NewHub builds an empty hub. Wire CallManager via SetCallManager after construction if needed.
func NewHub(auth Authenticator, touch Touch, loader func(ctx context.Context, userID string) (BridgeConfig, error)) *Hub {
	h := &Hub{
		Auth:          auth,
		Touch:         touch,
		SessionLoader: loader,
		clients:       make(map[string]*Client),
	}
	h.calls = NewCallManager(h)
	return h
}

// Calls returns the call manager.
func (h *Hub) Calls() *CallManager { return h.calls }

// CallsSnapshot implements modules.Provider SIPHub interface.
func (h *Hub) CallsSnapshot(userID string) []map[string]any {
	if h.calls == nil {
		return nil
	}
	return h.calls.Snapshot(userID)
}

// Online reports whether a device id is connected.
func (h *Hub) Online(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[deviceID]
	return ok
}

// OnlineCount for a user.
func (h *Hub) OnlineCount(userID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, c := range h.clients {
		if c.Device.UserID == userID {
			n++
		}
	}
	return n
}

// ListOnline devices for a user.
func (h *Hub) ListOnline(userID string) []store.SIPDevice {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []store.SIPDevice
	for _, c := range h.clients {
		if c.Device.UserID == userID {
			out = append(out, c.Device)
		}
	}
	return out
}

// HandleWS is GET /sip/ws?token=…
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	dev, err := h.Auth(r.Context(), token)
	if err != nil || dev == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("sip ws upgrade: %v", err)
		return
	}
	if h.Touch != nil {
		h.Touch(r.Context(), dev.ID)
	}

	c := &Client{
		Device: *dev,
		conn:   conn,
		send:   make(chan []byte, 64),
		hub:    h,
	}
	h.mu.Lock()
	if old, ok := h.clients[dev.ID]; ok {
		_ = old.conn.Close()
		delete(h.clients, dev.ID)
	}
	h.clients[dev.ID] = c
	h.mu.Unlock()

	log.Printf("sip device online id=%s name=%s user=%s", dev.ID, dev.Name, dev.UserID)
	go c.writePump()
	go c.readPump()

	// Immediate hello.ok with audio params (may be refined after user settings load).
	cfg, _ := h.loadConfig(context.Background(), dev.UserID)
	_ = c.SendJSON(map[string]any{
		"type":        "hello.ok",
		"server_time": time.Now().UTC().Format(time.RFC3339),
		"bridge_mode": cfg.BridgeMode,
		"device_id":   dev.Name,
		"audio": map[string]any{
			"format":   "pcm16",
			"rate":     cfg.AudioRate,
			"channels": 1,
		},
	})
}

func (h *Hub) loadConfig(ctx context.Context, userID string) (BridgeConfig, error) {
	if h.SessionLoader == nil {
		return BridgeConfig{Voice: "eve", AudioRate: 16000, AutoAnswer: true, BridgeMode: "realtime"}, nil
	}
	return h.SessionLoader(ctx, userID)
}

func (h *Hub) unregister(deviceID string) {
	h.mu.Lock()
	if c, ok := h.clients[deviceID]; ok {
		delete(h.clients, deviceID)
		close(c.send)
		_ = c.conn.Close()
	}
	h.mu.Unlock()
	if h.calls != nil {
		h.calls.OnDeviceDisconnect(deviceID)
	}
}

func (h *Hub) get(deviceID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[deviceID]
	return c, ok
}

// SendJSON to a device by id.
func (h *Hub) SendJSON(deviceID string, v any) error {
	c, ok := h.get(deviceID)
	if !ok {
		return errNotConnected
	}
	return c.SendJSON(v)
}

// SendBinary audio downlink.
func (h *Hub) SendBinary(deviceID string, pcm []byte) error {
	c, ok := h.get(deviceID)
	if !ok {
		return errNotConnected
	}
	return c.SendBinary(pcm)
}

// Hangup asks the phone to drop the cellular call.
func (h *Hub) Hangup(deviceID, callID string) error {
	return h.SendJSON(deviceID, map[string]any{"type": "call.hangup", "call_id": callID})
}

var errNotConnected = &hubError{"device not connected"}

type hubError struct{ s string }

func (e *hubError) Error() string { return e.s }

func (c *Client) SendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.enqueue(websocket.TextMessage, data)
}

func (c *Client) SendBinary(pcm []byte) error {
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	return c.enqueue(websocket.BinaryMessage, cp)
}

func (c *Client) enqueue(mt int, payload []byte) error {
	frame := make([]byte, 1+len(payload))
	frame[0] = byte(mt)
	copy(frame[1:], payload)
	select {
	case c.send <- frame:
		return nil
	default:
		return &hubError{"send buffer full"}
	}
}

func (c *Client) readPump() {
	defer c.hub.unregister(c.Device.ID)
	_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		if c.hub.Touch != nil {
			c.hub.Touch(context.Background(), c.Device.ID)
		}
		switch mt {
		case websocket.BinaryMessage:
			if c.hub.calls != nil {
				c.hub.calls.OnUplink(c.Device.ID, data)
			}
		case websocket.TextMessage:
			c.handleText(data)
		}
	}
}

func (c *Client) handleText(data []byte) {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	typ, _ := msg["type"].(string)
	switch typ {
	case "hello":
		cfg, _ := c.hub.loadConfig(context.Background(), c.Device.UserID)
		_ = c.SendJSON(map[string]any{
			"type":        "hello.ok",
			"server_time": time.Now().UTC().Format(time.RFC3339),
			"bridge_mode": cfg.BridgeMode,
			"device_id":   c.Device.Name,
			"audio": map[string]any{
				"format":   "pcm16",
				"rate":     cfg.AudioRate,
				"channels": 1,
			},
		})
	case "call.ringing":
		if c.hub.calls != nil {
			c.hub.calls.OnRinging(c.Device, str(msg["call_id"]), str(msg["from"]), str(msg["to"]))
		}
	case "call.answered":
		if c.hub.calls != nil {
			c.hub.calls.OnAnswered(c.Device, str(msg["call_id"]))
		}
	case "call.ended":
		if c.hub.calls != nil {
			c.hub.calls.End(c.Device.ID, str(msg["call_id"]), str(msg["reason"]))
		}
	case "ping":
		_ = c.SendJSON(map[string]any{"type": "pong", "ts": msg["ts"]})
	default:
		log.Printf("sip device %s unknown type %q", c.Device.Name, typ)
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case frame, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			mt := int(frame[0])
			payload := frame[1:]
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(mt, payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
