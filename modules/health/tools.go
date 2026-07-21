// Package health stores personal health profile, daily diary, and injuries/conditions for MCP clients.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns health_* tools.
func Factory(st *store.Store) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "health_status",
					Description: "Overview of this account's health data: baseline profile (height, weight, notes), " +
						"active/recovering issues, and recent daily log entries. Call first when the user mentions " +
						"how they feel, weight, injuries, or wants a health snapshot.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"log_days": map[string]any{"type": "integer", "description": "Recent diary days to include (default 7)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					limit := 7
					if v, ok := args["log_days"].(float64); ok && v > 0 {
						limit = int(v)
					}
					prof, hasProf, err := st.GetHealthProfile(ctx, userID)
					if err != nil {
						return "", err
					}
					issues, err := st.ListHealthIssues(ctx, userID, "", 20)
					if err != nil {
						return "", err
					}
					// Prefer open issues first in the summary.
					var open []map[string]any
					var closed []map[string]any
					for _, iss := range issues {
						row := issueOut(iss)
						stt := strings.ToLower(iss.Status)
						if stt == "resolved" {
							closed = append(closed, row)
						} else {
							open = append(open, row)
						}
					}
					logs, err := st.ListHealthLog(ctx, userID, "", "", limit)
					if err != nil {
						return "", err
					}
					logRows := make([]map[string]any, 0, len(logs))
					for _, e := range logs {
						logRows = append(logRows, logOut(e))
					}
					nLog, _ := st.CountHealthLog(ctx, userID)
					nIss, _ := st.CountHealthIssues(ctx, userID, "")
					return marshal(map[string]any{
						"profile":       profileOut(prof, hasProf),
						"open_issues":   open,
						"recent_issues": closed,
						"recent_log":    logRows,
						"counts":        map[string]any{"log_days": nLog, "issues": nIss, "open_issues": len(open)},
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_profile_set",
					Description: "Update baseline health profile: height_cm, weight_kg, freeform notes. " +
						"Only provided fields change. Weight is also updated when a daily log entry records weight.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"height_cm": map[string]any{"type": "number", "description": "Height in centimeters (e.g. 179)"},
							"weight_kg": map[string]any{"type": "number", "description": "Current weight in kg"},
							"notes":     map[string]any{"type": "string", "description": "Baseline notes (blood type, allergies, etc.)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					var height, weight *float64
					var notes *string
					if v, ok := floatArg(args, "height_cm"); ok {
						height = &v
					}
					if v, ok := floatArg(args, "weight_kg"); ok {
						weight = &v
					}
					if v, ok := args["notes"].(string); ok {
						notes = &v
					}
					if height == nil && weight == nil && notes == nil {
						return "", fmt.Errorf("provide at least one of height_cm, weight_kg, notes")
					}
					p, err := st.UpsertHealthProfile(ctx, userID, height, weight, notes)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "saved", "profile": profileOut(p, true)})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_log_list",
					Description: "List daily health diary entries (newest first). Optional from/to as YYYY-MM-DD.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"from":  map[string]any{"type": "string", "description": "YYYY-MM-DD inclusive"},
							"to":    map[string]any{"type": "string", "description": "YYYY-MM-DD inclusive"},
							"limit": map[string]any{"type": "integer"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					from, _ := args["from"].(string)
					to, _ := args["to"].(string)
					limit := 30
					if v, ok := args["limit"].(float64); ok && v > 0 {
						limit = int(v)
					}
					list, err := st.ListHealthLog(ctx, userID, from, to, limit)
					if err != nil {
						return "", err
					}
					rows := make([]map[string]any, 0, len(list))
					for _, e := range list {
						rows = append(rows, logOut(e))
					}
					return marshal(map[string]any{"count": len(rows), "entries": rows})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "health_log_get",
					Description: "Get one diary day (YYYY-MM-DD). Empty day = today (Europe/Madrid).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"day": map[string]any{"type": "string", "description": "YYYY-MM-DD; omit for today"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					day, _ := args["day"].(string)
					e, err := st.GetHealthLog(ctx, userID, day)
					if err == sql.ErrNoRows {
						return marshal(map[string]any{"status": "empty", "day": day, "hint": "use health_log_upsert to create"})
					}
					if err != nil {
						return "", err
					}
					return marshal(logOut(*e))
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_log_upsert",
					Description: "Create or update a daily health diary entry. day empty = today (Europe/Madrid). " +
						"Only provided fields change. Use append_note to add a bullet without wiping notes. " +
						"Fields: weight_kg, sleep, training, symptoms, pain, medication, notes. " +
						"When the user says how they feel today, log it here with detail.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"day":          map[string]any{"type": "string", "description": "YYYY-MM-DD; default today"},
							"weight_kg":    map[string]any{"type": "number"},
							"sleep":        map[string]any{"type": "string"},
							"training":     map[string]any{"type": "string"},
							"symptoms":     map[string]any{"type": "string"},
							"pain":         map[string]any{"type": "string"},
							"medication":   map[string]any{"type": "string"},
							"notes":        map[string]any{"type": "string", "description": "Replace notes entirely"},
							"append_note":  map[string]any{"type": "string", "description": "Append a line to notes"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					day, _ := args["day"].(string)
					var weight *float64
					if v, ok := floatArg(args, "weight_kg"); ok {
						weight = &v
					}
					sleep := strPtr(args, "sleep")
					training := strPtr(args, "training")
					symptoms := strPtr(args, "symptoms")
					pain := strPtr(args, "pain")
					medication := strPtr(args, "medication")
					notes := strPtr(args, "notes")
					appendNote, _ := args["append_note"].(string)
					if weight == nil && sleep == nil && training == nil && symptoms == nil && pain == nil &&
						medication == nil && notes == nil && strings.TrimSpace(appendNote) == "" {
						return "", fmt.Errorf("provide at least one field to set")
					}
					e, err := st.UpsertHealthLog(ctx, userID, day, weight, sleep, training, symptoms, pain, medication, notes, appendNote)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "saved", "entry": logOut(*e)})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "health_log_delete",
					Description: "Delete a diary day by YYYY-MM-DD. Prefer confirming with the user first.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"day": map[string]any{"type": "string"},
						},
						"required": []string{"day"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					day, _ := args["day"].(string)
					if strings.TrimSpace(day) == "" {
						return "", fmt.Errorf("day required")
					}
					if err := st.DeleteHealthLog(ctx, userID, day); err != nil {
						if err == sql.ErrNoRows {
							return "", fmt.Errorf("no entry for %s", day)
						}
						return "", err
					}
					return marshal(map[string]any{"status": "deleted", "day": day})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_issue_list",
					Description: "List injuries/conditions (historial). Optional status filter: active, recovering, resolved, chronic.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"status": map[string]any{"type": "string"},
							"limit":  map[string]any{"type": "integer"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					status, _ := args["status"].(string)
					limit := 50
					if v, ok := args["limit"].(float64); ok && v > 0 {
						limit = int(v)
					}
					list, err := st.ListHealthIssues(ctx, userID, status, limit)
					if err != nil {
						return "", err
					}
					rows := make([]map[string]any, 0, len(list))
					for _, iss := range list {
						rows = append(rows, issueOut(iss))
					}
					return marshal(map[string]any{"count": len(rows), "issues": rows})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "health_issue_get",
					Description: "Get full detail for one injury/condition by id.",
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
					iss, err := st.GetHealthIssue(ctx, userID, strings.TrimSpace(id))
					if err == sql.ErrNoRows {
						return "", fmt.Errorf("issue not found")
					}
					if err != nil {
						return "", err
					}
					return marshal(issueOut(*iss))
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_issue_add",
					Description: "Add an injury, condition, or medical event. status: active|recovering|resolved|chronic. " +
						"started_on as YYYY-MM-DD. Capture diagnosis, treatment, body_part, notes in detail.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":      map[string]any{"type": "string"},
							"status":     map[string]any{"type": "string"},
							"started_on": map[string]any{"type": "string"},
							"ended_on":   map[string]any{"type": "string"},
							"body_part":  map[string]any{"type": "string"},
							"diagnosis":  map[string]any{"type": "string"},
							"treatment":  map[string]any{"type": "string"},
							"notes":      map[string]any{"type": "string"},
						},
						"required": []string{"title"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					title, _ := args["title"].(string)
					iss := store.HealthIssue{
						UserID:     userID,
						Title:      title,
						Status:     strArg(args, "status"),
						StartedOn:  strArg(args, "started_on"),
						EndedOn:    strArg(args, "ended_on"),
						BodyPart:   strArg(args, "body_part"),
						Diagnosis:  strArg(args, "diagnosis"),
						Treatment:  strArg(args, "treatment"),
						Notes:      strArg(args, "notes"),
					}
					out, err := st.CreateHealthIssue(ctx, iss)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "created", "issue": issueOut(*out)})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "health_issue_update",
					Description: "Update an injury/condition by id. Only provided fields change. " +
						"append_notes adds a line without wiping notes. Use when recovery status changes.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":           map[string]any{"type": "string"},
							"title":        map[string]any{"type": "string"},
							"status":       map[string]any{"type": "string"},
							"started_on":   map[string]any{"type": "string"},
							"ended_on":     map[string]any{"type": "string"},
							"body_part":    map[string]any{"type": "string"},
							"diagnosis":    map[string]any{"type": "string"},
							"treatment":    map[string]any{"type": "string"},
							"notes":        map[string]any{"type": "string"},
							"append_notes": map[string]any{"type": "string"},
						},
						"required": []string{"id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					id, _ := args["id"].(string)
					if strings.TrimSpace(id) == "" {
						return "", fmt.Errorf("id required")
					}
					fields := map[string]string{}
					for _, k := range []string{"title", "status", "started_on", "ended_on", "body_part", "diagnosis", "treatment", "notes", "append_notes"} {
						if v, ok := args[k].(string); ok {
							fields[k] = v
						}
					}
					if len(fields) == 0 {
						return "", fmt.Errorf("provide at least one field to update")
					}
					out, err := st.UpdateHealthIssue(ctx, userID, strings.TrimSpace(id), fields)
					if err == sql.ErrNoRows {
						return "", fmt.Errorf("issue not found")
					}
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "updated", "issue": issueOut(*out)})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "health_issue_delete",
					Description: "Delete an injury/condition by id. Prefer confirming with the user first.",
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
					if err := st.DeleteHealthIssue(ctx, userID, strings.TrimSpace(id)); err != nil {
						if err == sql.ErrNoRows {
							return "", fmt.Errorf("issue not found")
						}
						return "", err
					}
					return marshal(map[string]any{"status": "deleted", "id": id})
				},
			},
		}
	}
}

func profileOut(p *store.HealthProfile, exists bool) map[string]any {
	out := map[string]any{
		"exists": exists,
		"notes":  "",
	}
	if p == nil {
		return out
	}
	out["notes"] = p.Notes
	if p.HeightCM != nil {
		out["height_cm"] = *p.HeightCM
		out["height_m"] = *p.HeightCM / 100
	}
	if p.WeightKG != nil {
		out["weight_kg"] = *p.WeightKG
	}
	if !p.UpdatedAt.IsZero() {
		out["updated_at"] = p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}

func logOut(e store.HealthLogEntry) map[string]any {
	out := map[string]any{
		"id": e.ID, "day": e.Day,
		"sleep": e.Sleep, "training": e.Training, "symptoms": e.Symptoms,
		"pain": e.Pain, "medication": e.Medication, "notes": e.Notes,
		"updated_at": e.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if e.WeightKG != nil {
		out["weight_kg"] = *e.WeightKG
	}
	return out
}

func issueOut(iss store.HealthIssue) map[string]any {
	return map[string]any{
		"id": iss.ID, "title": iss.Title, "status": iss.Status,
		"started_on": iss.StartedOn, "ended_on": iss.EndedOn, "body_part": iss.BodyPart,
		"diagnosis": iss.Diagnosis, "treatment": iss.Treatment, "notes": iss.Notes,
		"updated_at": iss.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func floatArg(args map[string]any, k string) (float64, bool) {
	v, ok := args[k]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		var f float64
		_, err := fmt.Sscanf(strings.TrimSpace(n), "%f", &f)
		return f, err == nil
	default:
		return 0, false
	}
}

func strArg(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return v
}

func strPtr(args map[string]any, k string) *string {
	v, ok := args[k].(string)
	if !ok {
		return nil
	}
	return &v
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
