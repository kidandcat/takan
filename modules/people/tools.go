// Package people stores people you know and your relationship context for MCP clients.
package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns people_* tools.
func Factory(st *store.Store) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "people_list",
					Description: "List people in your personal directory. Optional query filters by name, alias, relationship, tags, notes. " +
						"Use when you need who someone is or your relationship to them.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{"type": "string", "description": "Optional search text"},
							"limit": map[string]any{"type": "integer"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					q, _ := args["query"].(string)
					limit := 50
					if v, ok := args["limit"].(float64); ok && v > 0 {
						limit = int(v)
					}
					list, err := st.ListPeople(ctx, userID, q, limit)
					if err != nil {
						return "", err
					}
					rows := make([]map[string]any, 0, len(list))
					for _, p := range list {
						rows = append(rows, map[string]any{
							"id": p.ID, "name": p.Name, "relationship": p.Relationship,
							"tags": p.Tags, "aliases": p.Aliases,
							"context": trim(p.Context, 120),
						})
					}
					return marshal(map[string]any{"count": len(rows), "people": rows})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "people_get",
					Description: "Get full profile for a person by id or exact name/alias. " +
						"Includes relationship, context, notes, email, phone, contacts (key→value), tags.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":   map[string]any{"type": "string"},
							"name": map[string]any{"type": "string", "description": "Name or alias if id unknown"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					p, err := resolvePerson(ctx, st, userID, args)
					if err != nil {
						return "", err
					}
					return marshal(personOut(p))
				},
			},
			{
				Tool: mcp.Tool{
					Name: "people_add",
					Description: "Add a person you know. Capture name, relationship (friend/family/coworker/client/…), " +
						"context (how you relate), notes, aliases, tags, email, phone, contacts (arbitrary key→value), birthday if known.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":         map[string]any{"type": "string"},
							"relationship": map[string]any{"type": "string"},
							"context":      map[string]any{"type": "string", "description": "Your relationship / role in your life"},
							"notes":        map[string]any{"type": "string"},
							"aliases":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"tags":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"email":        map[string]any{"type": "string"},
							"phone":        map[string]any{"type": "string"},
							"contacts": map[string]any{
								"type":                 "object",
								"description":          "Arbitrary contact channels as key→value (e.g. {\"linkedin\":\"…\", \"telegram\":\"@x\", \"whatsapp\":\"+34…\"})",
								"additionalProperties": map[string]any{"type": "string"},
							},
							"birthday": map[string]any{"type": "string"},
						},
						"required": []string{"name"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					name, _ := args["name"].(string)
					p := store.Person{
						UserID:       userID,
						Name:         name,
						Relationship: strArg(args, "relationship"),
						Context:      strArg(args, "context"),
						Notes:        strArg(args, "notes"),
						Email:        strArg(args, "email"),
						Phone:        strArg(args, "phone"),
						Contacts:     mapArg(args, "contacts"),
						Birthday:     strArg(args, "birthday"),
						Aliases:      strListArg(args, "aliases"),
						Tags:         strListArg(args, "tags"),
					}
					// avoid exact duplicates by name
					if existing, err := st.FindPersonByName(ctx, userID, name); err == nil && existing != nil {
						return "", fmt.Errorf("person %q already exists (id=%s) — use people_update", existing.Name, existing.ID)
					}
					out, err := st.CreatePerson(ctx, p)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "created", "person": personOut(out)})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "people_update",
					Description: "Update a person by id or name. Only provided fields change. " +
						"Use append_notes to add a dated fact without wiping notes. " +
						"contacts replaces the whole map when provided; use contacts_merge to set/overwrite individual keys without clearing others.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":           map[string]any{"type": "string"},
							"name":         map[string]any{"type": "string", "description": "Lookup name if id unknown, or new name if renaming with id"},
							"new_name":     map[string]any{"type": "string"},
							"relationship": map[string]any{"type": "string"},
							"context":      map[string]any{"type": "string"},
							"notes":        map[string]any{"type": "string", "description": "Replace notes entirely"},
							"append_notes": map[string]any{"type": "string", "description": "Append a note line"},
							"email":        map[string]any{"type": "string"},
							"phone":        map[string]any{"type": "string"},
							"contacts": map[string]any{
								"type":                 "object",
								"description":          "Replace all contacts (key→value). Pass {} to clear.",
								"additionalProperties": map[string]any{"type": "string"},
							},
							"contacts_merge": map[string]any{
								"type":                 "object",
								"description":          "Merge into existing contacts (overwrite keys present; omit others). Empty string value deletes a key.",
								"additionalProperties": map[string]any{"type": "string"},
							},
							"birthday": map[string]any{"type": "string"},
							"aliases":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					p, err := resolvePerson(ctx, st, userID, args)
					if err != nil {
						return "", err
					}
					fields := map[string]string{}
					if v, ok := args["new_name"].(string); ok && strings.TrimSpace(v) != "" {
						fields["name"] = v
					}
					for _, k := range []string{"relationship", "context", "notes", "append_notes", "email", "phone", "birthday"} {
						if v, ok := args[k].(string); ok {
							fields[k] = v
						}
					}
					_, setAliases := args["aliases"]
					_, setTags := args["tags"]

					var contacts map[string]string
					setContacts := false
					if _, ok := args["contacts"]; ok {
						contacts = mapArg(args, "contacts")
						if contacts == nil {
							contacts = map[string]string{}
						}
						setContacts = true
					} else if _, ok := args["contacts_merge"]; ok {
						contacts = map[string]string{}
						for k, v := range p.Contacts {
							contacts[k] = v
						}
						for k, v := range mapArg(args, "contacts_merge") {
							if strings.TrimSpace(v) == "" {
								delete(contacts, k)
							} else {
								contacts[k] = v
							}
						}
						setContacts = true
					}

					out, err := st.UpdatePersonFields(ctx, userID, p.ID, fields, strListArg(args, "aliases"), strListArg(args, "tags"), setAliases, setTags, contacts, setContacts)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "updated", "person": personOut(out)})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "people_delete",
					Description: "Delete a person by id or name. Prefer confirming with the user first.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":   map[string]any{"type": "string"},
							"name": map[string]any{"type": "string"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					p, err := resolvePerson(ctx, st, userID, args)
					if err != nil {
						return "", err
					}
					if err := st.DeletePerson(ctx, userID, p.ID); err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "deleted", "id": p.ID, "name": p.Name})
				},
			},
		}
	}
}

func resolvePerson(ctx context.Context, st *store.Store, userID string, args map[string]any) (*store.Person, error) {
	if id, _ := args["id"].(string); strings.TrimSpace(id) != "" {
		p, err := st.GetPerson(ctx, userID, strings.TrimSpace(id))
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("person not found for id")
		}
		return p, err
	}
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("id or name required")
	}
	p, err := st.FindPersonByName(ctx, userID, name)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("person %q not found — use people_list or people_add", name)
	}
	return p, err
}

func personOut(p *store.Person) map[string]any {
	contacts := map[string]string{}
	for k, v := range p.Contacts {
		contacts[k] = v
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "aliases": p.Aliases, "relationship": p.Relationship,
		"context": p.Context, "notes": p.Notes, "tags": p.Tags, "birthday": p.Birthday,
		"email": p.Email, "phone": p.Phone, "contacts": contacts,
		"updated_at": p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func strArg(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return v
}

func mapArg(args map[string]any, k string) map[string]string {
	raw, ok := args[k]
	if !ok || raw == nil {
		return nil
	}
	out := map[string]string{}
	switch v := raw.(type) {
	case map[string]any:
		for key, val := range v {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			switch s := val.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					out[key] = strings.TrimSpace(s)
				}
			case float64:
				out[key] = fmt.Sprintf("%v", s)
			case bool:
				out[key] = fmt.Sprintf("%v", s)
			default:
				if val != nil {
					out[key] = strings.TrimSpace(fmt.Sprint(val))
				}
			}
		}
	case map[string]string:
		for key, val := range v {
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)
			if key != "" && val != "" {
				out[key] = val
			}
		}
	default:
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		var out []string
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out
	default:
		return nil
	}
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
