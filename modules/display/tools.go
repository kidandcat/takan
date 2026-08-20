package display

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// MaxHTMLBytes is the MCP/hub cap for a single display_show payload.
const MaxHTMLBytes = 1 << 20 // 1 MiB

// Factory returns display_* tools.
func Factory(st *store.Store, hub *agenthub.Hub) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "display_list",
					Description: "List remote screens (kiosk displays) for this Takan account. " +
						"Each display is a takan-agent that serves static HTML locally. " +
						"Use the name with display_show. The office kiosk is usually named oficina.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					ds, err := st.ListDisplays(ctx, userID)
					if err != nil {
						return "", err
					}
					if len(ds) == 0 {
						return "No displays registered. Open Takan panel → Display, pick a machine running takan-agent, and add a screen.", nil
					}
					type row struct {
						Name        string `json:"name"`
						Machine     string `json:"machine"`
						Online      bool   `json:"online"`
						Default     bool   `json:"default,omitempty"`
						LastShownAt string `json:"last_shown_at,omitempty"`
					}
					out := make([]row, 0, len(ds))
					for _, d := range ds {
						r := row{Name: d.Name, Machine: d.MachineName, Online: hub != nil && hub.Online(d.MachineID), Default: d.IsDefault}
						if d.LastShown != nil {
							r.LastShownAt = d.LastShown.UTC().Format("2006-01-02T15:04:05Z")
						}
						out = append(out, r)
					}
					return marshal(out)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "display_show",
					Description: "Push a static HTML document to a remote screen. The machine's takan-agent " +
						"serves it locally (kiosk browser). display is the screen name from display_list " +
						"(omit to use the default; the office kiosk is oficina). Pass a full HTML document. " +
						"Empty html clears to the idle page. Max 1 MiB.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"html": map[string]any{
								"type":        "string",
								"description": "Full HTML document to show (empty = idle/clear)",
							},
							"display": map[string]any{
								"type":        "string",
								"description": "Screen name from display_list (optional; default screen if omitted)",
							},
						},
						"required": []string{"html"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					html, _ := args["html"].(string)
					name, _ := args["display"].(string)
					if len(html) > MaxHTMLBytes {
						return "", fmt.Errorf("html too large (%d bytes, max %d)", len(html), MaxHTMLBytes)
					}
					d, err := resolveDisplay(ctx, st, userID, name)
					if err != nil {
						return "", err
					}
					if hub == nil {
						return "", fmt.Errorf("agent hub not available")
					}
					if !hub.Online(d.MachineID) {
						return "", fmt.Errorf("display %q is offline (machine %s)", d.Name, d.MachineName)
					}
					if err := hub.ShowHTML(ctx, userID, d.MachineName, html); err != nil {
						return "", err
					}
					_ = st.TouchDisplayShown(ctx, d.ID)
					return marshal(map[string]any{
						"status":  "shown",
						"display": d.Name,
						"machine": d.MachineName,
						"bytes":   len(html),
					})
				},
			},
		}
	}
}

func resolveDisplay(ctx context.Context, st *store.Store, userID, name string) (*store.Display, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		d, err := st.DisplayByUserAndName(ctx, userID, name)
		if err != nil {
			return nil, fmt.Errorf("unknown display %q — call display_list", name)
		}
		return d, nil
	}
	d, err := st.DefaultDisplay(ctx, userID)
	if err == nil {
		return d, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	all, err := st.ListDisplays(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(all) == 1 {
		return &all[0], nil
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no displays registered — open Takan panel → Display")
	}
	return nil, fmt.Errorf("pass display= (several screens; call display_list)")
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
