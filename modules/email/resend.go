package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

const resendBase = "https://api.resend.com"

func resendDo(ctx context.Context, apiKey, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, resendBase+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return raw, resp.StatusCode, nil
}

// FetchDomains lists domains from Resend and maps them to store.EmailDomain.
func FetchDomains(ctx context.Context, apiKey string) ([]store.EmailDomain, error) {
	raw, code, err := resendDo(ctx, apiKey, http.MethodGet, "/domains?limit=100", nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("resend domains %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Status       string `json:"status"`
			Capabilities struct {
				Sending   string `json:"sending"`
				Receiving string `json:"receiving"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var list []store.EmailDomain
	for _, d := range out.Data {
		list = append(list, store.EmailDomain{
			ID: d.ID, Name: d.Name, Status: d.Status,
			Sending: d.Capabilities.Sending, Receiving: d.Capabilities.Receiving,
		})
	}
	return list, nil
}

func sendResend(ctx context.Context, apiKey, from, to, subject, text, html string) (string, error) {
	payload := map[string]any{
		"from": from, "to": []string{to}, "subject": subject, "text": text,
	}
	if strings.TrimSpace(html) != "" {
		payload["html"] = html
	}
	raw, code, err := resendDo(ctx, apiKey, http.MethodPost, "/emails", payload)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("resend send %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.ID == "" {
		out.ID = "ok"
	}
	return out.ID, nil
}

// listReceived returns received email summaries from Resend.
func listReceived(ctx context.Context, apiKey string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	path := fmt.Sprintf("/emails/receiving?limit=%d", limit)
	raw, code, err := resendDo(ctx, apiKey, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("resend receiving list %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func getReceived(ctx context.Context, apiKey, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	path := "/emails/receiving/" + url.PathEscape(id)
	raw, code, err := resendDo(ctx, apiKey, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("resend receiving get %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func domainOfAddr(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	// strip display name: Name <a@b.com>
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		if j := strings.LastIndexByte(addr, '>'); j > i {
			addr = addr[i+1 : j]
		}
	}
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return strings.TrimSuffix(addr[i+1:], ">")
	}
	return ""
}

func addressesMatchEnabled(addrs any, enabled []string) bool {
	en := map[string]bool{}
	for _, d := range enabled {
		en[d] = true
	}
	switch v := addrs.(type) {
	case string:
		return en[domainOfAddr(v)]
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && en[domainOfAddr(s)] {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if en[domainOfAddr(s)] {
				return true
			}
		}
	}
	return false
}

func filterReceivedByDomains(list []map[string]any, enabled []string) []map[string]any {
	if len(enabled) == 0 {
		return nil
	}
	var out []map[string]any
	for _, item := range list {
		// Prefer "to" (inbound mailbox domain); fall back to from.
		if addressesMatchEnabled(item["to"], enabled) || addressesMatchEnabled(item["from"], enabled) {
			out = append(out, item)
		}
	}
	return out
}
