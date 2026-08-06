package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules/vault"
)

// --- vault ---

func vaultItemMeta(it store.VaultItem) map[string]any {
	urls := it.URLs
	if urls == nil {
		urls = []string{}
	}
	return map[string]any{
		"id":           it.ID,
		"name":         it.Name,
		"username":     it.Username,
		"urls":         urls,
		"folder":       it.Folder,
		"tags":         it.Tags,
		"favorite":     it.Favorite,
		"has_password": it.PasswordEnc != "",
		"has_totp":     it.TOTPEnc != "",
		"has_notes":    it.NotesEnc != "",
		"updated_at":   it.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) vaultList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	urlQ := r.URL.Query().Get("url")
	items, err := s.Store.SearchVaultItems(r.Context(), u.ID, q, urlQ, 200)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		rows = append(rows, vaultItemMeta(it))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

func (s *Server) vaultCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     string   `json:"name"`
		Username string   `json:"username"`
		Password string   `json:"password"`
		TOTP     string   `json:"totp"`
		Notes    string   `json:"notes"`
		URL      string   `json:"url"`
		URLs     []string `json:"urls"`
		Folder   string   `json:"folder"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	urls := body.URLs
	if body.URL != "" {
		urls = append([]string{body.URL}, urls...)
	}
	if body.Name == "" && len(urls) > 0 {
		body.Name = urls[0]
	}
	if body.Name == "" {
		s.writeErr(w, http.StatusBadRequest, "name or url required")
		return
	}
	var passEnc, totpEnc, notesEnc string
	var err error
	if body.Password != "" {
		passEnc, err = s.Box.Seal(body.Password)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.TOTP != "" {
		totpEnc, err = s.Box.Seal(body.TOTP)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.Notes != "" {
		notesEnc, err = s.Box.Seal(body.Notes)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	it, err := s.Store.CreateVaultItem(r.Context(), store.VaultItem{
		UserID: u.ID, Name: body.Name, Username: body.Username,
		PasswordEnc: passEnc, TOTPEnc: totpEnc, NotesEnc: notesEnc,
		URLs: urls, Folder: body.Folder,
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "vault", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"item": vaultItemMeta(it)})
}

func (s *Server) vaultDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteVaultItem(r.Context(), u.ID, r.PathValue("id")); err == sql.ErrNoRows {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func vaultSettingsJSON(cfg vault.Config) map[string]any {
	return map[string]any{
		"require_approval": cfg.RequireApproval,
	}
}

func (s *Server) vaultGetSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	cfg, err := vault.LoadConfig(r.Context(), s.Store, u.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, vaultSettingsJSON(cfg))
}

func (s *Server) vaultPatchSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	var body struct {
		RequireApproval *bool `json:"require_approval"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.RequireApproval == nil {
		s.writeErr(w, http.StatusBadRequest, "require_approval required")
		return
	}
	cfg, err := vault.LoadConfig(r.Context(), s.Store, u.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.RequireApproval = *body.RequireApproval
	if err := vault.SaveConfig(r.Context(), s.Store, u.ID, cfg); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, vaultSettingsJSON(cfg))
}

func grantJSON(g store.VaultGrant) map[string]any {
	return map[string]any{
		"id":         g.ID,
		"kind":       "vault_grant",
		"item_id":    g.ItemID,
		"item_name":  g.ItemName,
		"item_url":   g.ItemURL,
		"match_url":  g.MatchURL,
		"purpose":    g.Purpose,
		"fields":     g.Fields,
		"status":     g.Status,
		"mode":       g.Mode,
		"ttl":        g.TTLSeconds,
		"created_at": g.CreatedAt.UTC().Format(time.RFC3339),
		"title":      grantTitle(g),
		"subtitle":   g.Purpose,
	}
}

func grantTitle(g store.VaultGrant) string {
	if g.ItemName != "" {
		return "Read credentials · " + g.ItemName
	}
	if g.MatchURL != "" {
		return "Read credentials · " + g.MatchURL
	}
	return "Read credentials"
}

func (s *Server) vaultGrants(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	list, err := s.Store.ListVaultGrants(r.Context(), u.ID, status, 50)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(list))
	for _, g := range list {
		rows = append(rows, grantJSON(g))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grants": rows})
}

func (s *Server) vaultGrantApprove(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	var body struct {
		ItemID string `json:"item_id"`
	}
	_ = decodeJSON(r, &body)
	g, err := s.Store.DecideVaultGrant(r.Context(), u.ID, r.PathValue("id"), true, body.ItemID)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grant": grantJSON(g)})
}

func (s *Server) vaultGrantDeny(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	g, err := s.Store.DecideVaultGrant(r.Context(), u.ID, r.PathValue("id"), false, "")
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grant": grantJSON(g)})
}

// Approvals inbox — vault grants today; later other tool-call kinds share this shape.
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	list, err := s.Store.ListVaultGrants(r.Context(), u.ID, "pending", 50)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Also include recent non-pending for history
	recent, _ := s.Store.ListVaultGrants(r.Context(), u.ID, "", 30)
	pending := make([]map[string]any, 0)
	history := make([]map[string]any, 0)
	for _, g := range list {
		pending = append(pending, grantJSON(g))
	}
	for _, g := range recent {
		if g.Status == "pending" {
			continue
		}
		history = append(history, grantJSON(g))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"pending": pending,
		"history": history,
		"count":   len(pending),
	})
}

func (s *Server) approveAny(w http.ResponseWriter, r *http.Request) {
	// For now all approvals are vault grants
	s.vaultGrantApprove(w, r)
}

func (s *Server) denyAny(w http.ResponseWriter, r *http.Request) {
	s.vaultGrantDeny(w, r)
}

// --- people ---

func personJSON(p *store.Person) map[string]any {
	return map[string]any{
		"id":           p.ID,
		"name":         p.Name,
		"relationship": p.Relationship,
		"context":      p.Context,
		"notes":        p.Notes,
		"email":        p.Email,
		"phone":        p.Phone,
		"tags":         p.Tags,
		"aliases":      p.Aliases,
		"birthday":     p.Birthday,
		"contacts":     p.Contacts,
	}
}

func (s *Server) peopleList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	list, err := s.Store.ListPeople(r.Context(), u.ID, q, 100)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(list))
	for i := range list {
		rows = append(rows, personJSON(&list[i]))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"people": rows, "count": len(rows)})
}

func (s *Server) peopleGet(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	p, err := s.Store.GetPerson(r.Context(), u.ID, r.PathValue("id"))
	if err == sql.ErrNoRows {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"person": personJSON(p)})
}

func (s *Server) peopleCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name         string            `json:"name"`
		Relationship string            `json:"relationship"`
		Context      string            `json:"context"`
		Notes        string            `json:"notes"`
		Email        string            `json:"email"`
		Phone        string            `json:"phone"`
		Tags         []string          `json:"tags"`
		Aliases      []string          `json:"aliases"`
		Birthday     string            `json:"birthday"`
		Contacts     map[string]string `json:"contacts"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		s.writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	p, err := s.Store.CreatePerson(r.Context(), store.Person{
		UserID: u.ID, Name: body.Name, Relationship: body.Relationship,
		Context: body.Context, Notes: body.Notes, Email: body.Email, Phone: body.Phone,
		Tags: body.Tags, Aliases: body.Aliases, Birthday: body.Birthday, Contacts: body.Contacts,
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "people", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"person": personJSON(p)})
}

func (s *Server) peopleDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeletePerson(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- health ---

func (s *Server) healthSnapshot(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	prof, has, _ := s.Store.GetHealthProfile(r.Context(), u.ID)
	logs, _ := s.Store.ListHealthLog(r.Context(), u.ID, "", "", 14)
	issues, _ := s.Store.ListHealthIssues(r.Context(), u.ID, "", 20)
	profile := map[string]any{"has_data": has}
	if has && prof != nil {
		if prof.HeightCM != nil {
			profile["height_cm"] = *prof.HeightCM
		}
		if prof.WeightKG != nil {
			profile["weight_kg"] = *prof.WeightKG
		}
		profile["notes"] = prof.Notes
	}
	logRows := make([]map[string]any, 0, len(logs))
	for _, e := range logs {
		row := map[string]any{
			"day": e.Day, "sleep": e.Sleep, "training": e.Training,
			"symptoms": e.Symptoms, "pain": e.Pain, "medication": e.Medication, "notes": e.Notes,
		}
		if e.WeightKG != nil {
			row["weight_kg"] = *e.WeightKG
		}
		logRows = append(logRows, row)
	}
	issRows := make([]map[string]any, 0, len(issues))
	for _, iss := range issues {
		issRows = append(issRows, map[string]any{
			"id": iss.ID, "title": iss.Title, "status": iss.Status,
			"body_part": iss.BodyPart, "notes": iss.Notes,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"profile": profile,
		"log":     logRows,
		"issues":  issRows,
	})
}

// --- invites ---

func (s *Server) invitesList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	q, _ := s.Store.InviteQuota(r.Context(), u.ID)
	list, err := s.Store.ListInvites(r.Context(), u.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(list))
	for _, inv := range list {
		row := map[string]any{
			"id": inv.ID, "note": inv.Note,
			"used": inv.UsedBy != "", "created_at": inv.CreatedAt.UTC().Format(time.RFC3339),
		}
		if inv.ExpiresAt != nil {
			row["expires_at"] = inv.ExpiresAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	quota := map[string]any{}
	if q != nil {
		quota = map[string]any{
			"quota": q.Quota, "unlimited": q.Unlimited, "created": q.Created, "remaining": q.Remaining,
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"invites": rows, "quota": quota})
}

func (s *Server) invitesCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
		Days int    `json:"days"`
	}
	_ = decodeJSON(r, &body)
	ttl := 30 * 24 * time.Hour
	if body.Days > 0 {
		ttl = time.Duration(body.Days) * 24 * time.Hour
	}
	inv, err := s.Store.CreateInvite(r.Context(), u.ID, body.Note, ttl)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"id": inv.ID, "code": inv.RawCode, "note": inv.Note,
		"register_url": strings.TrimRight(s.PublicURL, "/") + "/register?invite=" + inv.RawCode,
	})
}
