package mercadona

import (
	"context"
	"fmt"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns mercadona_* tools (stub until full cart client is wired).
func Factory(st *store.Store) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{{
			Tool: mcp.Tool{
				Name: "mercadona_status",
				Description: "Check whether Mercadona credentials are configured for this Takan account. " +
					"Full cart tools will appear once credentials are saved in the panel.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				email, _, postal, ok, err := st.GetMercadonaCreds(ctx, userID)
				if err != nil {
					return "", err
				}
				if !ok {
					return "Mercadona module is ON but credentials are not set. Open the Takan panel → Mercadona and save email, password, and postal code.", nil
				}
				return fmt.Sprintf("Mercadona credentials configured for %s (CP %s). Full cart tools coming next; mercadona-mcp integration pending.", email, postal), nil
			},
		}}
	}
}
