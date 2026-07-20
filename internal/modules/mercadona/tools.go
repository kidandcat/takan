package mercadona

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kidandcat/mercadona-mcp/sdk"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Module holds shared Mercadona state (one DB, multi-tenant by Takan user id).
type Module struct {
	Takan *store.Store
	DB    *sql.DB
	Box   *sdk.Box
	Acc   *sdk.AccountStore // actually *accounts.Store via alias
}

// NewModule wires mercadona-mcp SDK pieces.
func NewModule(takan *store.Store, db *sql.DB, box *sdk.Box, publicURL string) *Module {
	return &Module{
		Takan: takan,
		DB:    db,
		Box:   box,
		Acc:   sdk.NewAccountStore(db, box, publicURL),
	}
}

// Factory returns mercadona_* tools when the module is enabled.
func (m *Module) Factory() func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return m.tools()
	}
}

func (m *Module) svc(userID string) *sdk.Service {
	return sdk.NewService(m.DB, userID, m.Acc)
}

func (m *Module) tools() []mcp.RegisteredTool {
	return []mcp.RegisteredTool{
		{
			Tool: mcp.Tool{
				Name: "mercadona_status",
				Description: "Check whether Mercadona is linked for this Takan account. " +
					"If not linked, open the Takan panel → Mercadona and save credentials.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				email, _, postal, ok, err := m.Takan.GetMercadonaCreds(ctx, userID)
				if err != nil {
					return "", err
				}
				linked := HasLinkedSession(ctx, m.DB, userID)
				if !ok && !linked {
					return "Mercadona module ON but not configured. Open the Takan panel → Mercadona.", nil
				}
				if !linked {
					return fmt.Sprintf("Credentials saved for %s (CP %s) but session not linked — re-save in the panel.", email, postal), nil
				}
				return fmt.Sprintf("Mercadona ready (%s, CP %s). Tools: search, add, list, remove, clear, resolve.", email, postal), nil
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_search",
				Description: "Search Mercadona products by free text. Prefer mercadona_add for cart adds.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
						"limit": map[string]any{"type": "integer"},
					},
					"required": []string{"query"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				q, _ := args["query"].(string)
				limit := int(numArg(args, "limit", 5))
				if limit > 20 {
					limit = 20
				}
				hits, err := m.svc(userID).Search(ctx, q, limit)
				if err != nil {
					return "", err
				}
				return marshal(hits)
			},
		},
		{
			Tool: mcp.Tool{
				Name: "mercadona_add",
				Description: "Add item to Mercadona cart by free-text name. " +
					"May return status=asked with options + pending_id → mercadona_resolve.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text":     map[string]any{"type": "string"},
						"quantity": map[string]any{"type": "number"},
					},
					"required": []string{"text"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				text, _ := args["text"].(string)
				res, err := m.svc(userID).Add(ctx, text, numArg(args, "quantity", 1))
				if err != nil {
					return "", err
				}
				return marshal(res)
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_add_by_id",
				Description: "Add known product_id to cart. Pass text= original query to learn alias.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"product_id": map[string]any{"type": "string"},
						"quantity":   map[string]any{"type": "number"},
						"text":       map[string]any{"type": "string"},
					},
					"required": []string{"product_id"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				id, _ := args["product_id"].(string)
				text, _ := args["text"].(string)
				res, err := m.svc(userID).AddByID(ctx, id, numArg(args, "quantity", 1), text)
				if err != nil {
					return "", err
				}
				return marshal(res)
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_resolve",
				Description: "Resolve pending mercadona_add. product_id=\"\" skips.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pending_id": map[string]any{"type": "integer"},
						"product_id": map[string]any{"type": "string"},
					},
					"required": []string{"pending_id", "product_id"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				pid := int64(numArg(args, "pending_id", 0))
				if pid <= 0 {
					return "", fmt.Errorf("pending_id required")
				}
				productID, _ := args["product_id"].(string)
				product, cart, err := m.svc(userID).Resolve(ctx, pid, productID)
				if err != nil {
					return "", err
				}
				if product == nil {
					return marshal(map[string]any{"status": "skipped"})
				}
				return marshal(map[string]any{
					"status": "added", "product": product, "cart_total": cart.Total, "preferred": true,
				})
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_remove",
				Description: "Remove cart line whose name contains text.",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required":   []string{"text"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				text, _ := args["text"].(string)
				removed, err := m.svc(userID).Remove(ctx, text)
				if err != nil {
					return "", err
				}
				return marshal(map[string]any{"status": "removed", "product": removed})
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_list",
				Description: "List current Mercadona cart lines and total.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				cart, err := m.svc(userID).GetCart(ctx)
				if err != nil {
					return "", err
				}
				return marshal(cart)
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_clear",
				Description: "Empty the Mercadona cart.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				if err := m.svc(userID).Clear(ctx); err != nil {
					return "", err
				}
				return "cart cleared", nil
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "mercadona_aliases_list",
				Description: "List saved free-text → product aliases.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				list, err := m.svc(userID).ListAliases(ctx)
				if err != nil {
					return "", err
				}
				return marshal(list)
			},
		},
	}
}

func (m *Module) requireLinked(ctx context.Context, userID string) error {
	if !HasLinkedSession(ctx, m.DB, userID) {
		return fmt.Errorf("Mercadona not linked — save credentials in the Takan panel (Mercadona section)")
	}
	return nil
}

func numArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return def
	}
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
