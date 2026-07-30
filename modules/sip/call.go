package sip

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// CallManager owns active calls across devices (one per device).
type CallManager struct {
	hub *Hub

	mu    sync.Mutex
	calls map[string]*activeCall // device id
}

type activeCall struct {
	LocalID  string
	DeviceID string
	UserID   string
	Name     string // device name
	From     string
	To       string
	State    string
	Started  time.Time
	cancel   context.CancelFunc
	session  *Session
}

func NewCallManager(h *Hub) *CallManager {
	return &CallManager{hub: h, calls: make(map[string]*activeCall)}
}

// Snapshot returns active calls for a user (empty userID = all).
func (m *CallManager) Snapshot(userID string) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.calls))
	for _, c := range m.calls {
		if userID != "" && c.UserID != userID {
			continue
		}
		out = append(out, map[string]any{
			"device_id":   c.DeviceID,
			"device_name": c.Name,
			"call_id":     c.LocalID,
			"from":        c.From,
			"to":          c.To,
			"state":       c.State,
			"started":     c.Started,
		})
	}
	return out
}

func (m *CallManager) OnRinging(dev store.SIPDevice, callID, from, to string) {
	m.mu.Lock()
	if existing, ok := m.calls[dev.ID]; ok {
		m.mu.Unlock()
		log.Printf("sip device %s already has call %s — ignore ring %s", dev.Name, existing.LocalID, callID)
		_ = m.hub.SendJSON(dev.ID, map[string]any{"type": "error", "message": "another call already active"})
		return
	}
	m.calls[dev.ID] = &activeCall{
		LocalID:  callID,
		DeviceID: dev.ID,
		UserID:   dev.UserID,
		Name:     dev.Name,
		From:     from,
		To:       to,
		State:    "ringing",
		Started:  time.Now(),
	}
	m.mu.Unlock()

	log.Printf("sip ring device=%s call=%s from=%s to=%s", dev.Name, callID, from, to)
	cfg, err := m.hub.loadConfig(context.Background(), dev.UserID)
	if err == nil && cfg.AutoAnswer {
		_ = m.hub.SendJSON(dev.ID, map[string]any{"type": "call.answer", "call_id": callID})
	}
}

func (m *CallManager) OnAnswered(dev store.SIPDevice, callID string) {
	m.mu.Lock()
	c, ok := m.calls[dev.ID]
	if !ok || c.LocalID != callID {
		m.mu.Unlock()
		return
	}
	c.State = "answered"
	m.mu.Unlock()

	log.Printf("sip answered device=%s call=%s", dev.Name, callID)
	go m.bridgeRealtime(dev, callID)
}

func (m *CallManager) bridgeRealtime(dev store.SIPDevice, callID string) {
	cfg, err := m.hub.loadConfig(context.Background(), dev.UserID)
	if err != nil || cfg.APIKey == "" {
		log.Printf("sip bridge: no xAI key for user %s", dev.UserID)
		_ = m.hub.SendJSON(dev.ID, map[string]any{
			"type":    "error",
			"message": "xAI API key not configured — open Takan panel → SIP",
		})
		_ = m.hub.Hangup(dev.ID, callID)
		m.End(dev.ID, callID, "no_api_key")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	c, ok := m.calls[dev.ID]
	if !ok || c.LocalID != callID {
		m.mu.Unlock()
		cancel()
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.cancel = cancel
	m.mu.Unlock()

	sessCfg := SessionConfig{
		Voice:        cfg.Voice,
		Instructions: cfg.Instructions,
		Model:        "grok-voice-latest",
		AudioRate:    cfg.AudioRate,
		OnDownlink: func(pcm []byte) {
			if err := m.hub.SendBinary(dev.ID, pcm); err != nil {
				cancel()
			}
		},
		OnEvent: func(t string) {
			switch t {
			case "session.created", "session.updated", "response.done",
				"input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "error":
				log.Printf("sip grok device=%s call=%s event=%s", dev.Name, callID, t)
			}
		},
	}

	session, err := DialDirect(ctx, cfg.APIKey, sessCfg)
	if err != nil {
		log.Printf("sip grok dial device=%s: %v", dev.Name, err)
		_ = m.hub.SendJSON(dev.ID, map[string]any{"type": "error", "message": "grok session failed: " + err.Error()})
		_ = m.hub.Hangup(dev.ID, callID)
		m.End(dev.ID, callID, "grok_dial_failed")
		return
	}

	m.mu.Lock()
	if c, ok := m.calls[dev.ID]; ok && c.LocalID == callID {
		c.session = session
		c.State = "bridged"
	} else {
		m.mu.Unlock()
		session.Close()
		cancel()
		return
	}
	m.mu.Unlock()

	log.Printf("sip bridged device=%s call=%s → grok realtime", dev.Name, callID)
	<-ctx.Done()
	session.Close()
	log.Printf("sip bridge closed device=%s call=%s", dev.Name, callID)
}

func (m *CallManager) OnUplink(deviceID string, pcm []byte) {
	m.mu.Lock()
	c, ok := m.calls[deviceID]
	var sess *Session
	if ok {
		sess = c.session
	}
	m.mu.Unlock()
	if sess == nil {
		return
	}
	_ = sess.SendPCM(pcm)
}

func (m *CallManager) End(deviceID, callID, reason string) {
	m.mu.Lock()
	c, ok := m.calls[deviceID]
	if !ok {
		m.mu.Unlock()
		return
	}
	if callID != "" && c.LocalID != callID {
		m.mu.Unlock()
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.session != nil {
		c.session.Close()
	}
	local := c.LocalID
	delete(m.calls, deviceID)
	m.mu.Unlock()
	log.Printf("sip call ended device=%s call=%s reason=%s", deviceID, local, reason)
}

func (m *CallManager) OnDeviceDisconnect(deviceID string) {
	m.End(deviceID, "", "device_disconnect")
}

// HangupUserCall ends a call for a device owned by userID.
func (m *CallManager) HangupUserCall(userID, deviceID, callID string) error {
	m.mu.Lock()
	c, ok := m.calls[deviceID]
	if !ok || c.UserID != userID {
		m.mu.Unlock()
		return errNotConnected
	}
	if callID != "" && c.LocalID != callID {
		m.mu.Unlock()
		return &hubError{"call_id mismatch"}
	}
	local := c.LocalID
	m.mu.Unlock()
	_ = m.hub.Hangup(deviceID, local)
	m.End(deviceID, local, "admin_hangup")
	return nil
}
