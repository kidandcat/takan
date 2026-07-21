package mercadona

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Module holds shared Mercadona state (one DB, multi-tenant by Takan user id).
type Module struct {
	Takan *store.Store
	DB    *sql.DB
	Box   *Box
	Acc   *AccountStore // actually *accounts.Store via alias
}

// NewModule wires Mercadona DB, crypto and tools.
func NewModule(takan *store.Store, db *sql.DB, box *Box, publicURL string) *Module {
	return &Module{
		Takan: takan,
		DB:    db,
		Box:   box,
		Acc:   NewAccountStore(db, box, publicURL),
	}
}

// Factory returns mercadona_* tools when the module is enabled.
func (m *Module) Factory() func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return m.tools()
	}
}

func (m *Module) svc(userID string) *Service {
	return NewService(m.DB, userID, m.Acc)
}

// Lean MCP surface: search, add (text|id|resolve), cart (list|remove|clear).
// Module readiness lives in takan_status (meta), not a per-module status tool.
func (m *Module) tools() []mcp.RegisteredTool {
	return []mcp.RegisteredTool{
		{
			Tool: mcp.Tool{
				Name:        "mercadona_search",
				Description: "Search Mercadona products by free text. Prefer mercadona_add to put items in the cart. " +
					"If not linked, configure Mercadona in the Takan panel (see takan_status).",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
						"limit": map[string]any{"type": "integer", "description": "Max hits (default 5, max 20)"},
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
				Description: "Add to cart. Modes: (1) text= free-name search/add; (2) product_id= direct add " +
					"(optional text= to learn alias); (3) pending_id+product_id= resolve a previous ambiguous add " +
					"(product_id empty string skips). May return status=asked with options + pending_id.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{
							"type":        "string",
							"description": "Free-text product name (mode 1), or original query when adding by id",
						},
						"product_id": map[string]any{
							"type":        "string",
							"description": "Known product id (mode 2) or choice when resolving pending (mode 3)",
						},
						"pending_id": map[string]any{
							"type":        "integer",
							"description": "Pending disambiguation id from a previous add (mode 3)",
						},
						"quantity": map[string]any{"type": "number", "description": "Quantity (default 1)"},
					},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				text, _ := args["text"].(string)
				productID, _ := args["product_id"].(string)
				text = strings.TrimSpace(text)
				productID = strings.TrimSpace(productID)
				qty := numArg(args, "quantity", 1)
				pendingID := int64(numArg(args, "pending_id", 0))

				// Mode 3: resolve pending disambiguation
				if pendingID > 0 {
					product, cart, err := m.svc(userID).Resolve(ctx, pendingID, productID)
					if err != nil {
						return "", err
					}
					if product == nil {
						return marshal(map[string]any{"status": "skipped"})
					}
					return marshal(map[string]any{
						"status": "added", "product": product, "cart_total": cart.Total, "preferred": true,
					})
				}

				// Mode 2: add by product id
				if productID != "" {
					res, err := m.svc(userID).AddByID(ctx, productID, qty, text)
					if err != nil {
						return "", err
					}
					return marshal(res)
				}

				// Mode 1: free-text add
				if text == "" {
					return "", fmt.Errorf("provide text= (name), product_id=, or pending_id=+product_id=")
				}
				res, err := m.svc(userID).Add(ctx, text, qty)
				if err != nil {
					return "", err
				}
				return marshal(res)
			},
		},
		{
			Tool: mcp.Tool{
				Name: "mercadona_cart",
				Description: "Cart operations. action=list (default) shows lines + total; " +
					"action=remove needs text= (match line name); action=clear empties the cart.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type":        "string",
							"enum":        []string{"list", "remove", "clear"},
							"description": "list | remove | clear (default list)",
						},
						"text": map[string]any{
							"type":        "string",
							"description": "Required for remove: substring match on cart line name",
						},
					},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if err := m.requireLinked(ctx, userID); err != nil {
					return "", err
				}
				action, _ := args["action"].(string)
				action = strings.ToLower(strings.TrimSpace(action))
				if action == "" {
					action = "list"
				}
				switch action {
				case "list":
					cart, err := m.svc(userID).GetCart(ctx)
					if err != nil {
						return "", err
					}
					return marshal(cart)
				case "remove":
					text, _ := args["text"].(string)
					text = strings.TrimSpace(text)
					if text == "" {
						return "", fmt.Errorf("text required for action=remove")
					}
					removed, err := m.svc(userID).Remove(ctx, text)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "removed", "product": removed})
				case "clear":
					if err := m.svc(userID).Clear(ctx); err != nil {
						return "", err
					}
					return "cart cleared", nil
				default:
					return "", fmt.Errorf(`action must be "list", "remove", or "clear"`)
				}
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
