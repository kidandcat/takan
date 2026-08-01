// Package vault is Takan's password manager: encrypted vault + agent grant broker.
// Secret plaintext is never returned from search — only after secrets_request → user approve → secrets_status.
package vault

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns secrets_* tools. box encrypts password/totp/notes at rest.
func Factory(st *store.Store, box *cryptox.Box) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "secrets_search",
					Description: "Search vault logins (metadata only: id, name, username, urls, folder, tags, favorite). " +
						"Never returns password, totp, or notes. Use secrets_request to obtain secrets after user approval. " +
						"Filter with query and/or url (host match).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{"type": "string", "description": "Text match on name, username, url, folder, tags"},
							"url":   map[string]any{"type": "string", "description": "Match by site URL / host"},
							"limit": map[string]any{"type": "integer", "description": "Max results (default 20)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					q, _ := args["query"].(string)
					u, _ := args["url"].(string)
					limit := 20
					if v, ok := args["limit"].(float64); ok && v > 0 {
						limit = int(v)
					}
					list, err := st.SearchVaultItems(ctx, userID, q, u, limit)
					if err != nil {
						return "", err
					}
					rows := make([]map[string]any, 0, len(list))
					for _, it := range list {
						rows = append(rows, itemMeta(it))
					}
					return marshal(map[string]any{"count": len(rows), "items": rows})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "secrets_request",
					Description: "Request access to secret fields of a vault item. Creates a pending grant the user must approve " +
						"(panel → Vault, later mobile biometrics). Pass item_id and/or url/query to match. " +
						"fields: username, password, totp, notes. mode: once (default, single consume) or session (until ttl). " +
						"Then poll secrets_status with grant_id until approved/denied/expired.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"item_id": map[string]any{"type": "string"},
							"url":     map[string]any{"type": "string", "description": "Match login by site URL"},
							"query":   map[string]any{"type": "string", "description": "Match by search text if no item_id"},
							"fields": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "username | password | totp | notes (default username+password)",
							},
							"purpose": map[string]any{"type": "string", "description": "Why the agent needs this (shown to user)"},
							"ttl":     map[string]any{"type": "integer", "description": "Seconds secret is usable after approve (default 120, max 3600)"},
							"mode":    map[string]any{"type": "string", "enum": []string{"once", "session"}, "description": "once=one-shot; session=until ttl"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					itemID, _ := args["item_id"].(string)
					matchURL, _ := args["url"].(string)
					matchQ, _ := args["query"].(string)
					purpose, _ := args["purpose"].(string)
					mode, _ := args["mode"].(string)
					ttl := 120
					if v, ok := args["ttl"].(float64); ok && v > 0 {
						ttl = int(v)
					}
					fields := strListArg(args, "fields")
					if itemID == "" && matchURL == "" && matchQ == "" {
						return "", fmt.Errorf("item_id, url, or query required")
					}
					g, err := st.CreateVaultGrant(ctx, store.VaultGrant{
						UserID: userID, ItemID: strings.TrimSpace(itemID),
						MatchURL: matchURL, MatchQuery: matchQ,
						Fields: fields, Purpose: purpose, Mode: mode, TTLSeconds: ttl,
					})
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"grant_id":    g.ID,
						"status":      g.Status,
						"mode":        g.Mode,
						"fields":      g.Fields,
						"item_id":     g.ItemID,
						"purpose":     g.Purpose,
						"ttl_seconds": g.TTLSeconds,
						"hint":        "User must approve in Takan panel → Vault (Pending grants). Poll secrets_status.",
					}
					if g.ItemID != "" {
						if it, err := st.GetVaultItem(ctx, userID, g.ItemID); err == nil {
							out["item"] = itemMeta(it)
						}
					} else {
						out["warning"] = "no item matched yet — user must pick item when approving"
					}
					return marshal(out)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "secrets_status",
					Description: "Poll a secrets_request grant. When status=approved (once mode), returns requested secret fields once " +
						"and marks the grant consumed. Session mode returns secrets until expires_at. " +
						"pending|denied|expired|consumed return no secrets. Do not echo passwords into chat or memory.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"grant_id": map[string]any{"type": "string"},
						},
						"required": []string{"grant_id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					grantID, _ := args["grant_id"].(string)
					if strings.TrimSpace(grantID) == "" {
						return "", fmt.Errorf("grant_id required")
					}
					g, err := st.GetVaultGrant(ctx, userID, strings.TrimSpace(grantID))
					if err == sql.ErrNoRows {
						return "", fmt.Errorf("grant not found")
					}
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"grant_id": g.ID,
						"status":   g.Status,
						"mode":     g.Mode,
						"fields":   g.Fields,
						"item_id":  g.ItemID,
						"purpose":  g.Purpose,
					}
					if g.ExpiresAt != nil {
						out["expires_at"] = g.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
					}
					switch g.Status {
					case "pending":
						out["hint"] = "waiting for user approval in panel → Vault"
						return marshal(out)
					case "denied", "expired", "consumed":
						return marshal(out)
					case "approved":
						if g.ItemID == "" {
							out["status"] = "error"
							out["error"] = "approved but no item_id"
							return marshal(out)
						}
						it, err := st.GetVaultItem(ctx, userID, g.ItemID)
						if err != nil {
							return "", err
						}
						secrets, err := decryptFields(box, it, g.Fields)
						if err != nil {
							return "", err
						}
						// once: consume so next poll has no secrets
						if g.Mode != "session" {
							if _, err := st.ConsumeVaultGrant(ctx, userID, g.ID); err != nil {
								return "", err
							}
							out["status"] = "consumed"
							out["consumed"] = true
						} else {
							out["status"] = "approved"
						}
						out["item"] = itemMeta(it)
						out["secrets"] = secrets
						out["warning"] = "Do not store secrets in memory, logs, or chat. Use immediately in the browser tool."
						return marshal(out)
					default:
						return marshal(out)
					}
				},
			},
			{
				Tool: mcp.Tool{
					Name: "secrets_store",
					Description: "Create or update a vault login (agent signup path). Writes do not require a grant. " +
						"Pass id to update; otherwise matches by url host if provided, else creates. " +
						"Empty password on update keeps the previous password. Returns metadata only.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":       map[string]any{"type": "string"},
							"name":     map[string]any{"type": "string"},
							"username": map[string]any{"type": "string"},
							"password": map[string]any{"type": "string"},
							"totp":     map[string]any{"type": "string", "description": "TOTP secret (base32), optional"},
							"notes":    map[string]any{"type": "string"},
							"url":      map[string]any{"type": "string", "description": "Primary URL"},
							"urls":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"folder":   map[string]any{"type": "string"},
							"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"favorite": map[string]any{"type": "boolean"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					id, _ := args["id"].(string)
					name, _ := args["name"].(string)
					username, _ := args["username"].(string)
					password, _ := args["password"].(string)
					totp, _ := args["totp"].(string)
					notes, _ := args["notes"].(string)
					folder, _ := args["folder"].(string)
					urls := strListArg(args, "urls")
					if u, _ := args["url"].(string); strings.TrimSpace(u) != "" {
						urls = append([]string{strings.TrimSpace(u)}, urls...)
					}
					urls = uniqStrings(urls)
					tags := strListArg(args, "tags")
					var fav *bool
					if v, ok := args["favorite"].(bool); ok {
						fav = &v
					}

					var passEnc, totpEnc, notesEnc string
					var err error
					if password != "" {
						passEnc, err = box.Seal(password)
						if err != nil {
							return "", err
						}
					}
					if totp != "" {
						totpEnc, err = box.Seal(totp)
						if err != nil {
							return "", err
						}
					}
					if notes != "" {
						notesEnc, err = box.Seal(notes)
						if err != nil {
							return "", err
						}
					}

					favVal := false
					if fav != nil {
						favVal = *fav
					}
					it := store.VaultItem{
						ID: id, UserID: userID, Name: name, Username: username,
						PasswordEnc: passEnc, TOTPEnc: totpEnc, NotesEnc: notesEnc,
						URLs: urls, Folder: folder, Tags: tags, Favorite: favVal,
					}
					out, created, err := st.UpsertVaultItem(ctx, it, true)
					if err != nil {
						return "", err
					}
					status := "updated"
					if created {
						status = "created"
					}
					return marshal(map[string]any{"status": status, "item": itemMeta(out)})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "secrets_generate",
					Description: "Generate a strong random password. Does not save to vault — pair with secrets_store if needed.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"length":         map[string]any{"type": "integer", "description": "Default 20, min 8, max 128"},
							"include_symbols": map[string]any{"type": "boolean", "description": "Default true"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					length := 20
					if v, ok := args["length"].(float64); ok && v > 0 {
						length = int(v)
					}
					symbols := true
					if v, ok := args["include_symbols"].(bool); ok {
						symbols = v
					}
					pw, err := generatePassword(length, symbols)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"password": pw, "length": len(pw)})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "secrets_delete",
					Description: "Delete a vault item by id. Prefer confirming with the user first.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id": map[string]any{"type": "string"},
						},
						"required": []string{"id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					id, _ := args["id"].(string)
					if strings.TrimSpace(id) == "" {
						return "", fmt.Errorf("id required")
					}
					if err := st.DeleteVaultItem(ctx, userID, strings.TrimSpace(id)); err == sql.ErrNoRows {
						return "", fmt.Errorf("item not found")
					} else if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "deleted", "id": id})
				},
			},
		}
	}
}

func itemMeta(it store.VaultItem) map[string]any {
	return map[string]any{
		"id":         it.ID,
		"name":       it.Name,
		"username":   it.Username,
		"urls":       it.URLs,
		"folder":     it.Folder,
		"tags":       it.Tags,
		"favorite":   it.Favorite,
		"updated_at": it.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"has_password": it.PasswordEnc != "",
		"has_totp":     it.TOTPEnc != "",
		"has_notes":    it.NotesEnc != "",
	}
}

func decryptFields(box *cryptox.Box, it store.VaultItem, fields []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range fields {
		switch f {
		case "username":
			out["username"] = it.Username
		case "password":
			if it.PasswordEnc == "" {
				out["password"] = ""
				continue
			}
			p, err := box.Open(it.PasswordEnc)
			if err != nil {
				return nil, fmt.Errorf("decrypt password: %w", err)
			}
			out["password"] = p
		case "totp":
			if it.TOTPEnc == "" {
				out["totp"] = ""
				continue
			}
			p, err := box.Open(it.TOTPEnc)
			if err != nil {
				return nil, fmt.Errorf("decrypt totp: %w", err)
			}
			out["totp"] = p
		case "notes":
			if it.NotesEnc == "" {
				out["notes"] = ""
				continue
			}
			p, err := box.Open(it.NotesEnc)
			if err != nil {
				return nil, fmt.Errorf("decrypt notes: %w", err)
			}
			out["notes"] = p
		}
	}
	return out, nil
}

func generatePassword(length int, symbols bool) (string, error) {
	if length < 8 {
		length = 8
	}
	if length > 128 {
		length = 128
	}
	alphabet := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if symbols {
		alphabet += "!@#$%^&*()-_=+[]{}"
	}
	var b strings.Builder
	b.Grow(length)
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

func strListArg(args map[string]any, k string) []string {
	raw, ok := args[k]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
