// Package memory is short-lived per-user working memory for MCP clients.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns memory_get / memory_set tools.
func Factory(st *store.Store) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "memory_get",
					Description: "Read this account's short-lived working memory (plain text). " +
						"Optional key filters lines containing that substring.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"key": map[string]any{"type": "string", "description": "Optional line filter"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					content, updated, ok, err := st.GetMemory(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok || strings.TrimSpace(content) == "" {
						return "(empty memory)", nil
					}
					key, _ := args["key"].(string)
					key = strings.TrimSpace(key)
					if key != "" {
						var lines []string
						for _, line := range strings.Split(content, "\n") {
							if strings.Contains(strings.ToLower(line), strings.ToLower(key)) {
								lines = append(lines, line)
							}
						}
						if len(lines) == 0 {
							return fmt.Sprintf("(no lines matching %q)", key), nil
						}
						content = strings.Join(lines, "\n")
					}
					meta := ""
					if !updated.IsZero() {
						meta = "\n\n— updated " + updated.UTC().Format(time.RFC3339)
					}
					return content + meta, nil
				},
			},
			{
				Tool: mcp.Tool{
					Name: "memory_set",
					Description: "Replace this account's short-lived working memory with content. " +
						"Full replace (not append). Use for temporary context, not secrets.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
						},
						"required": []string{"content"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					content, _ := args["content"].(string)
					if err := st.SetMemory(ctx, userID, content); err != nil {
						return "", err
					}
					b, _ := json.MarshalIndent(map[string]any{
						"status": "saved",
						"bytes":  len(content),
					}, "", "  ")
					return string(b), nil
				},
			},
		}
	}
}
