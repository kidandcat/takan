// Package service is the stateful façade used by the MCP tools: session
// persistence, ambiguity resolution, preferred/alias bookkeeping, and cart
// mutations.
//
// Each Service instance is scoped to one accountID ("local" for stdio env mode,
// or a hosted connection id).
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kidandcat/takan/modules/mercadona/client"
	"github.com/kidandcat/takan/modules/mercadona/store"
)

// Service owns the Mercadona client + local SQLite state for one account.
type Service struct {
	db        *sql.DB
	client    *client.Client
	accountID string

	// Optional: when set, session load/save goes through the account row
	// (encrypted tokens) instead of mercadona_session / env.
	accountStore AccountStore
}

// AccountStore loads and persists Mercadona session tokens for a hosted account.
type AccountStore interface {
	LoadSession(ctx context.Context, accountID string) (*client.Session, error)
	SaveSession(ctx context.Context, accountID string, sess *client.Session) error
	Touch(ctx context.Context, accountID string) error
}

// New wraps db with a Mercadona client for the given account (use store.LocalAccountID for stdio).
func New(db *sql.DB, accountID string) *Service {
	if accountID == "" {
		accountID = store.LocalAccountID
	}
	return &Service{db: db, client: client.New(), accountID: accountID}
}

// WithAccountStore enables hosted multi-tenant session persistence.
func (s *Service) WithAccountStore(as AccountStore) *Service {
	s.accountStore = as
	return s
}

// Client returns the underlying Mercadona HTTP client (for postal resolution etc.).
func (s *Service) Client() *client.Client { return s.client }

// AccountID returns the scoped account id.
func (s *Service) AccountID() string { return s.accountID }

// AddResult is the structured result for mercadona_add.
type AddResult struct {
	Status    string           `json:"status"` // "added" | "asked" | "not_found" | "unavailable"
	Product   *client.Product  `json:"product,omitempty"`
	Quantity  float64          `json:"quantity,omitempty"`
	CartTotal float64          `json:"cart_total,omitempty"`
	PendingID int64            `json:"pending_id,omitempty"`
	Options   []client.Product `json:"options,omitempty"`
	AliasText string           `json:"alias_text,omitempty"`
	Message   string           `json:"message,omitempty"`
	// Preferred is true when the product was auto-picked from the preferred list.
	Preferred bool `json:"preferred,omitempty"`
}

// Alias is a saved free-text → product mapping (exact query match).
type Alias struct {
	ID          int64  `json:"id"`
	Alias       string `json:"alias"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	UseCount    int    `json:"use_count"`
}

// Preferred is a product the user has chosen before (any free-text query).
type Preferred struct {
	ID          int64  `json:"id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	UseCount    int    `json:"use_count"`
}

// SearchHit is a catalog product annotated with whether it is preferred for this account.
type SearchHit struct {
	client.Product
	Preferred bool `json:"preferred"`
}

func (s *Service) getSession(ctx context.Context, forceRefresh bool) (*client.Session, error) {
	if s.accountStore != nil {
		if !forceRefresh {
			sess, err := s.accountStore.LoadSession(ctx, s.accountID)
			if err == nil {
				return sess, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		// Hosted accounts must already have tokens; re-auth via refresh only.
		sess, err := s.accountStore.LoadSession(ctx, s.accountID)
		if err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
		if sess.RefreshToken == "" {
			return nil, fmt.Errorf("session expired — reconnect your Mercadona account on the website")
		}
		refreshed, err := s.client.Refresh(ctx, sess.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = sess.RefreshToken
		}
		if refreshed.CustomerID == "" {
			refreshed.CustomerID = sess.CustomerID
		}
		if err := s.accountStore.SaveSession(ctx, s.accountID, refreshed); err != nil {
			return nil, err
		}
		return refreshed, nil
	}

	// Local / stdio mode.
	if !forceRefresh {
		var sess client.Session
		err := s.db.QueryRowContext(ctx, `
			SELECT access_token, COALESCE(refresh_token,''), customer_id
			FROM mercadona_session WHERE id = 1
		`).Scan(&sess.AccessToken, &sess.RefreshToken, &sess.CustomerID)
		if err == nil {
			return &sess, nil
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("session load: %w", err)
		}
	}
	return s.authenticateLocal(ctx)
}

func (s *Service) authenticateLocal(ctx context.Context) (*client.Session, error) {
	if rt := strings.TrimSpace(os.Getenv("MERCADONA_REFRESH_TOKEN")); rt != "" {
		sess, err := s.client.Refresh(ctx, rt)
		if err != nil {
			return nil, fmt.Errorf("refresh token: %w", err)
		}
		if sess.RefreshToken == "" {
			sess.RefreshToken = rt
		}
		if err := s.saveLocalSession(ctx, sess); err != nil {
			return nil, err
		}
		return sess, nil
	}

	access := strings.TrimSpace(os.Getenv("MERCADONA_ACCESS_TOKEN"))
	customer := strings.TrimSpace(os.Getenv("MERCADONA_CUSTOMER_ID"))
	if access != "" && customer != "" {
		sess := &client.Session{
			AccessToken:  access,
			RefreshToken: strings.TrimSpace(os.Getenv("MERCADONA_REFRESH_TOKEN")),
			CustomerID:   customer,
		}
		if err := s.saveLocalSession(ctx, sess); err != nil {
			return nil, err
		}
		return sess, nil
	}

	user := strings.TrimSpace(os.Getenv("MERCADONA_USER"))
	pass := os.Getenv("MERCADONA_PASS")
	if user == "" || pass == "" {
		return nil, fmt.Errorf("no credentials: set MERCADONA_REFRESH_TOKEN, or MERCADONA_ACCESS_TOKEN+MERCADONA_CUSTOMER_ID, or MERCADONA_USER+MERCADONA_PASS")
	}
	sess, err := s.client.Login(ctx, user, pass)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if err := s.saveLocalSession(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Service) saveLocalSession(ctx context.Context, sess *client.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mercadona_session (id, access_token, refresh_token, customer_id, updated_at)
		VALUES (1, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			customer_id = excluded.customer_id,
			updated_at = CURRENT_TIMESTAMP
	`, sess.AccessToken, sess.RefreshToken, sess.CustomerID)
	if err != nil {
		return fmt.Errorf("session save: %w", err)
	}
	return nil
}

func (s *Service) saveSession(ctx context.Context, sess *client.Session) error {
	if s.accountStore != nil {
		return s.accountStore.SaveSession(ctx, s.accountID, sess)
	}
	return s.saveLocalSession(ctx, sess)
}

func withRetry[T any](s *Service, ctx context.Context, op func(*client.Session) (T, error)) (T, error) {
	var zero T
	sess, err := s.getSession(ctx, false)
	if err != nil {
		return zero, err
	}
	out, err := op(sess)
	if err == nil {
		if s.accountStore != nil {
			_ = s.accountStore.Touch(ctx, s.accountID)
		}
		return out, nil
	}
	if !errors.Is(err, client.ErrUnauthorized) {
		return zero, err
	}
	if sess.RefreshToken != "" {
		refreshed, rerr := s.client.Refresh(ctx, sess.RefreshToken)
		if rerr == nil {
			if refreshed.RefreshToken == "" {
				refreshed.RefreshToken = sess.RefreshToken
			}
			if refreshed.CustomerID == "" {
				refreshed.CustomerID = sess.CustomerID
			}
			if serr := s.saveSession(ctx, refreshed); serr != nil {
				return zero, serr
			}
			return op(refreshed)
		}
	}
	sess, err = s.getSession(ctx, true)
	if err != nil {
		return zero, err
	}
	return op(sess)
}

// Search products by free text (no auth required for the index itself).
// Hits that the user has preferred before are flagged preferred=true and sorted first.
func (s *Service) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query required")
	}
	if limit <= 0 {
		limit = 5
	}
	hits, err := s.client.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	prefSet, err := s.preferredIDSet(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, len(hits))
	for i, h := range hits {
		out[i] = SearchHit{Product: h, Preferred: prefSet[h.ID]}
	}
	// Preferred first so agents see the user's usual pick without scanning.
	for i := 0; i < len(out); i++ {
		if !out[i].Preferred {
			continue
		}
		for j := i; j > 0 && !out[j-1].Preferred; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// GetCart returns the current cart.
func (s *Service) GetCart(ctx context.Context) (*client.Cart, error) {
	return withRetry(s, ctx, func(sess *client.Session) (*client.Cart, error) {
		return s.client.GetCart(ctx, sess)
	})
}

// Add adds text (qty) to the cart. See AddResult.Status for outcomes.
//
// Preference order:
//  1. Exact free-text alias for this query
//  2. Search hits: auto-add when a single clear match, or when exactly one hit
//     is on the preferred list (product chosen in a past session)
//  3. Otherwise status=asked with options (agent must call mercadona_resolve)
func (s *Service) Add(ctx context.Context, text string, qty float64) (*AddResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("text required")
	}
	if qty <= 0 {
		qty = 1
	}
	lowered := strings.ToLower(text)

	var aliasID, aliasName string
	err := s.db.QueryRowContext(ctx, `
		SELECT product_id, product_name FROM grocery_aliases
		WHERE account_id = ? AND alias = ?
	`, s.accountID, lowered).Scan(&aliasID, &aliasName)
	if err == nil {
		avail, aerr := s.productAvailable(ctx, aliasID)
		if aerr != nil {
			return nil, aerr
		}
		if avail {
			product := client.Product{ID: aliasID, DisplayName: aliasName}
			cart, err := s.addLine(ctx, product, qty)
			if err != nil {
				return nil, err
			}
			_, _ = s.db.ExecContext(ctx, `
				UPDATE grocery_aliases SET use_count = use_count + 1, last_used = CURRENT_TIMESTAMP
				WHERE account_id = ? AND alias = ?
			`, s.accountID, lowered)
			_ = s.upsertPreferred(ctx, aliasID, aliasName)
			for _, l := range cart.Lines {
				if l.Product.ID == aliasID {
					p := l.Product
					return &AddResult{Status: "added", Product: &p, Quantity: l.Quantity, CartTotal: cart.Total, Preferred: true}, nil
				}
			}
			return &AddResult{Status: "added", Product: &product, Quantity: qty, CartTotal: cart.Total, Preferred: true}, nil
		}
		_, _ = s.db.ExecContext(ctx, `DELETE FROM grocery_aliases WHERE account_id = ? AND alias = ?`, s.accountID, lowered)
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("alias lookup: %w", err)
	}

	hits, err := s.client.Search(ctx, text, 5)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return &AddResult{Status: "not_found", AliasText: text, Message: "no products matched"}, nil
	}

	// Single clear match: name contains the query.
	if len(hits) == 1 && strings.Contains(strings.ToLower(hits[0].DisplayName), lowered) {
		return s.addChosen(ctx, hits[0], qty, lowered, false)
	}

	// Exactly one preferred product among the hits → auto-pick (no re-ask).
	prefHits, err := s.filterPreferred(ctx, hits)
	if err != nil {
		return nil, err
	}
	if len(prefHits) == 1 {
		res, err := s.addChosen(ctx, prefHits[0], qty, lowered, true)
		if err != nil {
			return nil, err
		}
		if res.Status == "added" {
			res.Message = "auto-selected preferred product"
		}
		return res, nil
	}

	optsJSON, _ := json.Marshal(hits)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO grocery_pending (account_id, alias_text, options_json) VALUES (?, ?, ?)
	`, s.accountID, lowered, string(optsJSON))
	if err != nil {
		return nil, fmt.Errorf("save pending: %w", err)
	}
	pendingID, _ := res.LastInsertId()
	msg := "ambiguous — pick a product_id and call mercadona_resolve"
	if len(prefHits) > 1 {
		msg = "ambiguous (several preferred) — pick a product_id and call mercadona_resolve"
	}
	return &AddResult{
		Status:    "asked",
		PendingID: pendingID,
		Options:   hits,
		AliasText: lowered,
		Message:   msg,
	}, nil
}

// addChosen adds a product, learns alias + preferred, and returns the cart result.
func (s *Service) addChosen(ctx context.Context, hit client.Product, qty float64, aliasText string, fromPreferred bool) (*AddResult, error) {
	avail, aerr := s.productAvailable(ctx, hit.ID)
	if aerr != nil {
		return nil, aerr
	}
	if !avail {
		return &AddResult{Status: "unavailable", Product: &hit, AliasText: aliasText, Message: "product not available in your zone"}, nil
	}
	cart, err := s.addLine(ctx, hit, qty)
	if err != nil {
		return nil, err
	}
	if aliasText != "" {
		if err := s.upsertAlias(ctx, aliasText, hit.ID, hit.DisplayName); err != nil {
			return nil, err
		}
	}
	if err := s.upsertPreferred(ctx, hit.ID, hit.DisplayName); err != nil {
		return nil, err
	}
	for _, l := range cart.Lines {
		if l.Product.ID == hit.ID {
			p := l.Product
			return &AddResult{Status: "added", Product: &p, Quantity: l.Quantity, CartTotal: cart.Total, Preferred: fromPreferred}, nil
		}
	}
	return &AddResult{Status: "added", Product: &hit, Quantity: qty, CartTotal: cart.Total, Preferred: fromPreferred}, nil
}

// AddByID adds a known product id to the cart (skips search).
// If text is non-empty, it is saved as an alias for future mercadona_add calls.
// The product is always added to the preferred list after a successful add.
func (s *Service) AddByID(ctx context.Context, productID string, qty float64, text string) (*AddResult, error) {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil, fmt.Errorf("product_id required")
	}
	if qty <= 0 {
		qty = 1
	}
	avail, err := s.productAvailable(ctx, productID)
	if err != nil {
		return nil, err
	}
	if !avail {
		return &AddResult{Status: "unavailable", Product: &client.Product{ID: productID}, Message: "product not available in your zone"}, nil
	}
	product := client.Product{ID: productID, DisplayName: productID}
	cart, err := s.addLine(ctx, product, qty)
	if err != nil {
		return nil, err
	}
	var added *client.Product
	for _, l := range cart.Lines {
		if l.Product.ID == productID {
			p := l.Product
			added = &p
			break
		}
	}
	if added == nil {
		added = &product
	}
	name := added.DisplayName
	if name == "" {
		name = productID
	}
	if err := s.upsertPreferred(ctx, productID, name); err != nil {
		return nil, err
	}
	if t := strings.ToLower(strings.TrimSpace(text)); t != "" {
		if err := s.upsertAlias(ctx, t, productID, name); err != nil {
			return nil, err
		}
	}
	return &AddResult{Status: "added", Product: added, Quantity: qty, CartTotal: cart.Total}, nil
}

// Resolve completes a previously ambiguous Add.
// Choosing a product learns both the free-text alias and preferred product.
func (s *Service) Resolve(ctx context.Context, pendingID int64, productID string) (*client.Product, *client.Cart, error) {
	var (
		aliasText  string
		optsJSON   string
		resolvedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT alias_text, options_json, resolved_at FROM grocery_pending
		WHERE id = ? AND account_id = ?
	`, pendingID, s.accountID).Scan(&aliasText, &optsJSON, &resolvedAt)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("pending %d not found", pendingID)
	}
	if err != nil {
		return nil, nil, err
	}
	if resolvedAt.Valid {
		return nil, nil, fmt.Errorf("pending %d already resolved", pendingID)
	}
	var options []client.Product
	if err := json.Unmarshal([]byte(optsJSON), &options); err != nil {
		return nil, nil, fmt.Errorf("decode options: %w", err)
	}
	if productID == "" {
		_, _ = s.db.ExecContext(ctx, `UPDATE grocery_pending SET resolved_at = CURRENT_TIMESTAMP WHERE id = ? AND account_id = ?`, pendingID, s.accountID)
		return nil, nil, nil
	}
	var chosen *client.Product
	for i := range options {
		if options[i].ID == productID {
			chosen = &options[i]
			break
		}
	}
	if chosen == nil {
		return nil, nil, fmt.Errorf("product_id %s not among pending options", productID)
	}
	avail, aerr := s.productAvailable(ctx, chosen.ID)
	if aerr != nil {
		return nil, nil, aerr
	}
	if !avail {
		return chosen, nil, fmt.Errorf("%q not available in your zone: %w", chosen.DisplayName, client.ErrProductUnavailable)
	}
	cart, err := s.addLine(ctx, *chosen, 1)
	if err != nil {
		return nil, nil, err
	}
	if err := s.upsertAlias(ctx, aliasText, chosen.ID, chosen.DisplayName); err != nil {
		return nil, nil, err
	}
	if err := s.upsertPreferred(ctx, chosen.ID, chosen.DisplayName); err != nil {
		return nil, nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE grocery_pending SET resolved_at = CURRENT_TIMESTAMP WHERE id = ? AND account_id = ?`, pendingID, s.accountID)
	return chosen, cart, nil
}

// Remove deletes the cart line whose display name matches text (substring).
func (s *Service) Remove(ctx context.Context, text string) (*client.Product, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("text required")
	}
	lowered := strings.ToLower(text)
	cart, err := s.GetCart(ctx)
	if err != nil {
		return nil, err
	}
	var matches []int
	for i, l := range cart.Lines {
		if strings.Contains(strings.ToLower(l.Product.DisplayName), lowered) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no cart line matches %q", text)
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, idx := range matches {
			names = append(names, cart.Lines[idx].Product.DisplayName)
		}
		return nil, fmt.Errorf("%d cart lines match %q: %s", len(matches), text, strings.Join(names, "; "))
	}
	idx := matches[0]
	removed := cart.Lines[idx].Product
	cart.Lines = append(cart.Lines[:idx], cart.Lines[idx+1:]...)
	_, err = withRetry(s, ctx, func(sess *client.Session) (*client.Cart, error) {
		return s.client.UpdateCart(ctx, sess, cart)
	})
	if err != nil {
		return nil, err
	}
	return &removed, nil
}

// Clear empties the cart.
func (s *Service) Clear(ctx context.Context) error {
	cart, err := s.GetCart(ctx)
	if err != nil {
		return err
	}
	cart.Lines = nil
	_, err = withRetry(s, ctx, func(sess *client.Session) (*client.Cart, error) {
		return s.client.UpdateCart(ctx, sess, cart)
	})
	return err
}

// ListAliases returns saved free-text aliases ordered by use.
func (s *Service) ListAliases(ctx context.Context) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, alias, product_id, product_name, use_count
		FROM grocery_aliases WHERE account_id = ?
		ORDER BY use_count DESC, last_used DESC
	`, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.ID, &a.Alias, &a.ProductID, &a.ProductName, &a.UseCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAlias removes one alias by id.
func (s *Service) DeleteAlias(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM grocery_aliases WHERE id = ? AND account_id = ?`, id, s.accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %d not found", id)
	}
	return nil
}

// ListPreferred returns products the user has chosen before, most-used first.
func (s *Service) ListPreferred(ctx context.Context) ([]Preferred, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, product_id, product_name, use_count
		FROM grocery_preferred WHERE account_id = ?
		ORDER BY use_count DESC, last_used DESC
	`, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Preferred
	for rows.Next() {
		var p Preferred
		if err := rows.Scan(&p.ID, &p.ProductID, &p.ProductName, &p.UseCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePreferred removes one preferred product by id (row id from ListPreferred).
func (s *Service) DeletePreferred(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM grocery_preferred WHERE id = ? AND account_id = ?`, id, s.accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("preferred %d not found", id)
	}
	return nil
}

// FormatCart renders a cart as a human-readable multi-line string.
func FormatCart(cart *client.Cart) string {
	if cart == nil || len(cart.Lines) == 0 {
		return "(empty cart)"
	}
	var b strings.Builder
	for _, l := range cart.Lines {
		qty := int(l.Quantity)
		if l.Product.Packaging != "" {
			fmt.Fprintf(&b, "- %dx %s (%s) — %.2f€\n", qty, l.Product.DisplayName, l.Product.Packaging, l.Product.UnitPrice)
		} else {
			fmt.Fprintf(&b, "- %dx %s — %.2f€\n", qty, l.Product.DisplayName, l.Product.UnitPrice)
		}
	}
	fmt.Fprintf(&b, "\nTotal: %.2f€", cart.Total)
	return b.String()
}

func (s *Service) productAvailable(ctx context.Context, productID string) (bool, error) {
	return withRetry(s, ctx, func(sess *client.Session) (bool, error) {
		return s.client.CheckAvailability(ctx, sess, productID)
	})
}

func (s *Service) addLine(ctx context.Context, product client.Product, qty float64) (*client.Cart, error) {
	cart, err := s.GetCart(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range cart.Lines {
		if cart.Lines[i].Product.ID == product.ID {
			cart.Lines[i].Quantity += qty
			found = true
			break
		}
	}
	if !found {
		cart.Lines = append(cart.Lines, client.CartLine{Quantity: qty, Product: product})
	}
	return withRetry(s, ctx, func(sess *client.Session) (*client.Cart, error) {
		return s.client.UpdateCart(ctx, sess, cart)
	})
}

func (s *Service) upsertAlias(ctx context.Context, alias, productID, productName string) error {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" || productID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO grocery_aliases (account_id, alias, product_id, product_name) VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, alias) DO UPDATE SET
			product_id = excluded.product_id,
			product_name = excluded.product_name,
			use_count = grocery_aliases.use_count + 1,
			last_used = CURRENT_TIMESTAMP
	`, s.accountID, alias, productID, productName)
	if err != nil {
		return fmt.Errorf("upsert alias: %w", err)
	}
	return nil
}

func (s *Service) upsertPreferred(ctx context.Context, productID, productName string) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil
	}
	if productName == "" {
		productName = productID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO grocery_preferred (account_id, product_id, product_name) VALUES (?, ?, ?)
		ON CONFLICT(account_id, product_id) DO UPDATE SET
			product_name = excluded.product_name,
			use_count = grocery_preferred.use_count + 1,
			last_used = CURRENT_TIMESTAMP
	`, s.accountID, productID, productName)
	if err != nil {
		return fmt.Errorf("upsert preferred: %w", err)
	}
	return nil
}

func (s *Service) preferredIDSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT product_id FROM grocery_preferred WHERE account_id = ?
	`, s.accountID)
	if err != nil {
		return nil, fmt.Errorf("preferred list: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Service) filterPreferred(ctx context.Context, hits []client.Product) ([]client.Product, error) {
	prefSet, err := s.preferredIDSet(ctx)
	if err != nil {
		return nil, err
	}
	if len(prefSet) == 0 {
		return nil, nil
	}
	var out []client.Product
	for _, h := range hits {
		if prefSet[h.ID] {
			out = append(out, h)
		}
	}
	return out, nil
}
