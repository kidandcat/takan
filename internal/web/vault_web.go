package web

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules/vault"
)

type vaultItemView struct {
	ID, Name, Username, URL, Folder, TagsLine string
	Favorite, HasPassword, HasTOTP            bool
	OTPCode, OTPRemaining                     string // current code when HasTOTP (panel only)
}

type vaultGrantView struct {
	ID, ItemID, ItemName, ItemURL, Purpose, Status, Mode, Fields, Created string
	TTL                                                                   int
}

func (s *Server) dashVault(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "vault", "Vault", "vault.html")
}

func (s *Server) createVaultItem(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	totp := strings.TrimSpace(r.FormValue("totp"))
	notes := r.FormValue("notes")
	folder := strings.TrimSpace(r.FormValue("folder"))
	rawURL := strings.TrimSpace(r.FormValue("url"))
	var urls []string
	if rawURL != "" {
		urls = []string{rawURL}
	}
	if name == "" && rawURL != "" {
		name = rawURL
	}
	if name == "" {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery("name or url required"), http.StatusFound)
		return
	}
	it, err := s.sealVaultForm(name, username, password, totp, notes, folder, urls, u.ID)
	if err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if _, err := s.Store.CreateVaultItem(r.Context(), it); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "vault", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/vault?flash=Login+saved", http.StatusFound)
}

func (s *Server) updateVaultItem(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	id := r.PathValue("id")
	name := strings.TrimSpace(r.FormValue("name"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	totp := strings.TrimSpace(r.FormValue("totp"))
	notes := r.FormValue("notes")
	folder := strings.TrimSpace(r.FormValue("folder"))
	rawURL := strings.TrimSpace(r.FormValue("url"))
	var urls []string
	if rawURL != "" {
		urls = []string{rawURL}
	}
	var passEnc, totpEnc, notesEnc string
	var err error
	if password != "" {
		passEnc, err = s.Box.Seal(password)
		if err != nil {
			http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
	}
	if totp != "" {
		totpEnc, err = s.Box.Seal(totp)
		if err != nil {
			http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
	}
	if notes != "" {
		notesEnc, err = s.Box.Seal(notes)
		if err != nil {
			http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
	}
	if _, err := s.Store.UpdateVaultItem(r.Context(), u.ID, id, name, username, passEnc, totpEnc, notesEnc, folder, urls, nil, nil, true, false, true); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/vault?flash=Login+updated", http.StatusFound)
}

func (s *Server) deleteVaultItem(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.DeleteVaultItem(r.Context(), u.ID, id); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/vault?flash=Login+deleted", http.StatusFound)
}

func (s *Server) approveVaultGrant(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	id := r.PathValue("id")
	itemID := strings.TrimSpace(r.FormValue("item_id"))
	if _, err := s.Store.DecideVaultGrant(r.Context(), u.ID, id, true, itemID); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/vault?flash=Grant+approved", http.StatusFound)
}

func (s *Server) denyVaultGrant(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.DecideVaultGrant(r.Context(), u.ID, id, false, ""); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/vault?flash=Grant+denied", http.StatusFound)
}

func (s *Server) importVaultCSV(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseMultipartForm(12 << 20); err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery("upload failed"), http.StatusFound)
		return
	}
	f, _, err := r.FormFile("csv")
	if err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery("csv file required"), http.StatusFound)
		return
	}
	defer f.Close()
	n, err := s.importChromeCSV(r.Context(), u.ID, f)
	if err != nil {
		http.Redirect(w, r, "/dashboard/vault?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "vault", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/vault?flash="+url.QueryEscape(fmt.Sprintf("Imported %d login(s)", n)), http.StatusFound)
}

func (s *Server) sealVaultForm(name, username, password, totp, notes, folder string, urls []string, userID string) (store.VaultItem, error) {
	var passEnc, totpEnc, notesEnc string
	var err error
	if password != "" {
		passEnc, err = s.Box.Seal(password)
		if err != nil {
			return store.VaultItem{}, err
		}
	}
	if totp != "" {
		totp = normalizeVaultTOTPInput(totp)
		totpEnc, err = s.Box.Seal(totp)
		if err != nil {
			return store.VaultItem{}, err
		}
	}
	if notes != "" {
		notesEnc, err = s.Box.Seal(notes)
		if err != nil {
			return store.VaultItem{}, err
		}
	}
	return store.VaultItem{
		UserID: userID, Name: name, Username: username,
		PasswordEnc: passEnc, TOTPEnc: totpEnc, NotesEnc: notesEnc,
		URLs: urls, Folder: folder,
	}, nil
}

func normalizeVaultTOTPInput(raw string) string {
	return vault.NormalizeTOTPForStore(raw)
}

// fillVaultOTP populates current TOTP codes for panel display.
func (s *Server) fillVaultOTP(items []vaultItemView, encSecrets map[string]string) {
	now := time.Now()
	for i := range items {
		sec := encSecrets[items[i].ID]
		if sec == "" {
			continue
		}
		plain, err := s.Box.Open(sec)
		if err != nil {
			continue
		}
		code, rem, _, err := vault.CurrentOTPCode(plain, now)
		if err != nil {
			continue
		}
		items[i].OTPCode = code
		items[i].OTPRemaining = fmt.Sprintf("%d", rem)
	}
}

// importChromeCSV imports Chrome / Google Password Manager CSV exports.
// Flexible headers: name, url, username, password, note/notes.
func (s *Server) importChromeCSV(ctx context.Context, userID string, r io.Reader) (int, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return 0, fmt.Errorf("read csv header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		key = strings.TrimPrefix(key, "\ufeff")
		idx[key] = i
	}
	// Accept common aliases
	col := func(names ...string) int {
		for _, n := range names {
			if i, ok := idx[n]; ok {
				return i
			}
		}
		return -1
	}
	iName := col("name", "title")
	iURL := col("url", "login_uri", "uri")
	iUser := col("username", "login_username", "login")
	iPass := col("password", "login_password")
	iNote := col("note", "notes", "login_note")
	if iURL < 0 && iUser < 0 && iPass < 0 {
		return 0, fmt.Errorf("unrecognized csv headers (need url/username/password)")
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	n := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, fmt.Errorf("csv row: %w", err)
		}
		name := get(rec, iName)
		rawURL := get(rec, iURL)
		username := get(rec, iUser)
		password := get(rec, iPass)
		notes := get(rec, iNote)
		if rawURL == "" && username == "" && password == "" {
			continue
		}
		if name == "" {
			name = rawURL
		}
		if name == "" {
			name = username
		}
		var urls []string
		if rawURL != "" {
			urls = []string{rawURL}
		}
		it, err := s.sealVaultForm(name, username, password, "", notes, "", urls, userID)
		if err != nil {
			return n, err
		}
		if _, _, err := s.Store.UpsertVaultItem(ctx, it, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
