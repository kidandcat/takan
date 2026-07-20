// Package client is a small HTTP client for the unofficial Mercadona web-store
// API (tienda.mercadona.es) and the public Algolia products index used by the
// website frontend.
//
// Unofficial. Mercadona has no public developer API. Credentials and request
// rates are the caller's responsibility.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Public Algolia credentials embedded in the Mercadona SPA for every anonymous
// visitor. Scoped to the products index; safe to ship. They rotate occasionally
// — if search starts failing, re-read them from the live SPA bundle.
const (
	DefaultAlgoliaApp = "7UZJKL1DJ0"
	DefaultAlgoliaKey = "9d8f2e39e90df472b4f2e559a116fe17"
	DefaultBaseURL    = "https://tienda.mercadona.es/api"
	DefaultIndex      = "products_prod"
)

// ErrUnauthorized means the access token is no longer valid; callers should
// refresh (or re-login) and retry once.
var ErrUnauthorized = errors.New("mercadona: unauthorized (401)")

// ErrProductUnavailable means a product is not deliverable in the warehouse
// bound to the session (postal-code zone).
var ErrProductUnavailable = errors.New("mercadona: product unavailable in zone")

// Session is the authenticated customer context.
type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	CustomerID   string `json:"customer_id"`
}

// Product is the slim projection we expose to agents.
type Product struct {
	ID              string  `json:"id"`
	DisplayName     string  `json:"display_name"`
	Packaging       string  `json:"packaging,omitempty"`
	Brand           string  `json:"brand,omitempty"`
	UnitPrice       float64 `json:"unit_price,omitempty"`
	BulkPrice       float64 `json:"bulk_price,omitempty"`
	ReferenceFormat string  `json:"reference_format,omitempty"`
	Thumbnail       string  `json:"thumbnail,omitempty"`
	Slug            string  `json:"slug,omitempty"`
}

// CartLine is one row in the cart.
type CartLine struct {
	Quantity float64 `json:"quantity"`
	Product  Product `json:"product"`
}

// Cart is GET/PUT /customers/{id}/cart/.
type Cart struct {
	ID      string     `json:"id"`
	Version int        `json:"version"`
	Lines   []CartLine `json:"lines"`
	Total   float64    `json:"total"`
}

// ProductDetail is the warehouse-scoped view of one product.
type ProductDetail struct {
	Available bool
	Thumbnail string
}

// Client talks to Mercadona + Algolia.
type Client struct {
	http       *http.Client
	baseURL    string
	algoliaApp string
	algoliaKey string
	index      string
}

// New returns a Client with public defaults.
func New() *Client {
	return &Client{
		http:       &http.Client{Timeout: 30 * time.Second},
		baseURL:    DefaultBaseURL,
		algoliaApp: DefaultAlgoliaApp,
		algoliaKey: DefaultAlgoliaKey,
		index:      DefaultIndex,
	}
}

// Login posts username+password to /auth/tokens/.
// Note: some accounts / periods require a reCAPTCHA token; if login fails with
// 4xx, prefer setting MERCADONA_REFRESH_TOKEN from a browser session instead.
func (c *Client) Login(ctx context.Context, username, password string) (*Session, error) {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth/tokens/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: login http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mercadona: login http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return decodeSession(raw)
}

// PostalChange is the result of setting a customer's postal code.
type PostalChange struct {
	PostalCode string
	Warehouse  string
	// WarehouseChanged is true when Mercadona reassigned the cart warehouse.
	WarehouseChanged bool
}

// ChangePostalCode sets the delivery postal code for the authenticated session.
// PUT /postal-codes/actions/change-pc/ with {"new_postal_code":"28022"}.
// Warehouse is read from the response header x-customer-wh.
func (c *Client) ChangePostalCode(ctx context.Context, s *Session, postalCode string) (*PostalChange, error) {
	postalCode = strings.TrimSpace(postalCode)
	if postalCode == "" {
		return nil, fmt.Errorf("mercadona: empty postal_code")
	}
	body, _ := json.Marshal(map[string]string{"new_postal_code": postalCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/postal-codes/actions/change-pc/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build change-pc: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req, s)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: change-pc http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mercadona: change-pc http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		WarehouseChanged bool `json:"warehouse_changed"`
	}
	_ = json.Unmarshal(raw, &parsed)
	wh := strings.TrimSpace(resp.Header.Get("x-customer-wh"))
	pc := strings.TrimSpace(resp.Header.Get("x-customer-pc"))
	if pc == "" {
		pc = postalCode
	}
	return &PostalChange{
		PostalCode:       pc,
		Warehouse:        wh,
		WarehouseChanged: parsed.WarehouseChanged,
	}, nil
}

// ResolvePostalCode validates a postal code anonymously and returns the warehouse id.
// Same endpoint as ChangePostalCode without auth — used for UI validation.
func (c *Client) ResolvePostalCode(ctx context.Context, postalCode string) (*PostalChange, error) {
	postalCode = strings.TrimSpace(postalCode)
	if postalCode == "" {
		return nil, fmt.Errorf("mercadona: empty postal_code")
	}
	body, _ := json.Marshal(map[string]string{"new_postal_code": postalCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/postal-codes/actions/change-pc/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build resolve-pc: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: resolve-pc http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mercadona: invalid postal code %q (%d): %s", postalCode, resp.StatusCode, truncate(string(raw), 200))
	}
	return &PostalChange{
		PostalCode: strings.TrimSpace(resp.Header.Get("x-customer-pc")),
		Warehouse:  strings.TrimSpace(resp.Header.Get("x-customer-wh")),
	}, nil
}

// Refresh exchanges a refresh_token for a new access token (and rotated refresh).
// POST /auth/tokens/ with {"refresh_token": "..." } — no captcha, headless-friendly.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("mercadona: empty refresh_token")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth/tokens/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: refresh http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mercadona: refresh http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return decodeSession(raw)
}

// GetCart fetches the current shopping cart.
func (c *Client) GetCart(ctx context.Context, s *Session) (*Cart, error) {
	url := fmt.Sprintf("%s/customers/%s/cart/", c.baseURL, s.CustomerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("mercadona: build cart: %w", err)
	}
	c.auth(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: cart http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mercadona: cart http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return decodeCart(resp.Body)
}

// UpdateCart replaces ALL cart lines (not a delta). Callers must pass the full desired state.
func (c *Client) UpdateCart(ctx context.Context, s *Session, cart *Cart) (*Cart, error) {
	type putLine struct {
		ProductID string  `json:"product_id"`
		Quantity  float64 `json:"quantity"`
	}
	lines := make([]putLine, 0, len(cart.Lines))
	for _, l := range cart.Lines {
		lines = append(lines, putLine{ProductID: l.Product.ID, Quantity: l.Quantity})
	}
	body, _ := json.Marshal(map[string]any{
		"id":      cart.ID,
		"version": cart.Version,
		"lines":   lines,
	})
	url := fmt.Sprintf("%s/customers/%s/cart/", c.baseURL, s.CustomerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build cart update: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: cart update http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mercadona: cart update http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return decodeCart(resp.Body)
}

// Search runs an Algolia query against the public products index (no auth).
func (c *Client) Search(ctx context.Context, query string, hitsPerPage int) ([]Product, error) {
	if hitsPerPage <= 0 {
		hitsPerPage = 5
	}
	body, _ := json.Marshal(map[string]any{
		"query":       query,
		"hitsPerPage": hitsPerPage,
	})
	url := fmt.Sprintf("https://%s-dsn.algolia.net/1/indexes/%s/query", strings.ToLower(c.algoliaApp), c.index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mercadona: build search: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Algolia-Application-Id", c.algoliaApp)
	req.Header.Set("X-Algolia-API-Key", c.algoliaKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: search http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mercadona: search http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Hits []rawProduct `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("mercadona: decode search: %w", err)
	}
	out := make([]Product, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		out = append(out, h.toProduct())
	}
	return out, nil
}

// GetProductDetail fetches warehouse-scoped product info (availability + live thumbnail).
func (c *Client) GetProductDetail(ctx context.Context, s *Session, productID string) (*ProductDetail, error) {
	url := fmt.Sprintf("%s/products/%s/", c.baseURL, productID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("mercadona: build product: %w", err)
	}
	c.auth(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mercadona: product http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return &ProductDetail{Available: false}, nil
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mercadona: product http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var pr struct {
		Published *bool  `json:"published"`
		Status    string `json:"status"`
		Thumbnail string `json:"thumbnail"`
		Photos    []struct {
			Thumbnail string `json:"thumbnail"`
			Regular   string `json:"regular"`
		} `json:"photos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("mercadona: decode product: %w", err)
	}
	available := (pr.Published == nil || *pr.Published) && !strings.EqualFold(pr.Status, "unavailable")
	thumb := strings.TrimSpace(pr.Thumbnail)
	if thumb == "" {
		for _, p := range pr.Photos {
			if t := strings.TrimSpace(p.Thumbnail); t != "" {
				thumb = t
				break
			}
			if r := strings.TrimSpace(p.Regular); r != "" {
				thumb = r
				break
			}
		}
	}
	return &ProductDetail{Available: available, Thumbnail: thumb}, nil
}

// CheckAvailability reports whether productID is deliverable in the session zone.
func (c *Client) CheckAvailability(ctx context.Context, s *Session, productID string) (bool, error) {
	d, err := c.GetProductDetail(ctx, s, productID)
	if err != nil {
		return false, err
	}
	return d.Available, nil
}

func (c *Client) auth(req *http.Request, s *Session) {
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
}

func decodeSession(raw []byte) (*Session, error) {
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("mercadona: decode session: %w", err)
	}
	// Some responses use customer_uuid instead of customer_id.
	if s.CustomerID == "" {
		var alt struct {
			CustomerUUID string `json:"customer_uuid"`
		}
		_ = json.Unmarshal(raw, &alt)
		s.CustomerID = alt.CustomerUUID
	}
	if s.AccessToken == "" || s.CustomerID == "" {
		return nil, fmt.Errorf("mercadona: session missing access_token/customer_id")
	}
	return &s, nil
}

// --- raw decoding ---

type rawPriceInstructions struct {
	UnitPrice       string `json:"unit_price"`
	BulkPrice       string `json:"bulk_price"`
	ReferenceFormat string `json:"reference_format"`
}

type rawProduct struct {
	ID                string               `json:"id"`
	DisplayName       string               `json:"display_name"`
	Packaging         string               `json:"packaging"`
	Brand             string               `json:"brand"`
	Slug              string               `json:"slug"`
	Thumbnail         string               `json:"thumbnail"`
	PriceInstructions rawPriceInstructions `json:"price_instructions"`
}

func (p rawProduct) toProduct() Product {
	return Product{
		ID:              p.ID,
		DisplayName:     p.DisplayName,
		Packaging:       p.Packaging,
		Brand:           p.Brand,
		UnitPrice:       parseFloat(p.PriceInstructions.UnitPrice),
		BulkPrice:       parseFloat(p.PriceInstructions.BulkPrice),
		ReferenceFormat: p.PriceInstructions.ReferenceFormat,
		Thumbnail:       p.Thumbnail,
		Slug:            p.Slug,
	}
}

type rawCart struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Summary struct {
		Total json.RawMessage `json:"total"`
	} `json:"summary"`
	Lines []struct {
		Quantity float64    `json:"quantity"`
		Product  rawProduct `json:"product"`
	} `json:"lines"`
}

func decodeCart(body io.Reader) (*Cart, error) {
	var raw rawCart
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("mercadona: decode cart: %w", err)
	}
	out := &Cart{
		ID:      raw.ID,
		Version: raw.Version,
		Total:   parseRawFloat(raw.Summary.Total),
		Lines:   make([]CartLine, 0, len(raw.Lines)),
	}
	for _, l := range raw.Lines {
		out.Lines = append(out.Lines, CartLine{Quantity: l.Quantity, Product: l.Product.toProduct()})
	}
	return out, nil
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func parseRawFloat(raw json.RawMessage) float64 {
	return parseFloat(strings.Trim(strings.TrimSpace(string(raw)), `"`))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
