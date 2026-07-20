package files

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3Config is S3-compatible object storage (OVH, AWS, MinIO, R2…).
type S3Config struct {
	Endpoint  string // e.g. https://s3.gra.io.cloud.ovh.net
	Region    string
	Bucket    string
	Prefix    string // e.g. takan-files/
	AccessKey string
	SecretKey string
	// PublicBase is the public URL prefix for objects, e.g.
	// https://storage.gra.cloud.ovh.net/v1/AUTH_xxx/bucket  or a CDN.
	// If empty, uses virtual-hosted https://<bucket>.<endpoint-host>/<key>
	PublicBase string
}

// BlobStore uploads objects with SigV4.
type BlobStore struct {
	host         string
	endpointHost string
	prefix       string
	region       string
	access       string
	secret       string
	publicBase   string
	http         *http.Client
}

// NewBlobStore validates cfg.
func NewBlobStore(cfg S3Config) (*BlobStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("files storage: Endpoint, Bucket, AccessKey and SecretKey are required")
	}
	ep := cfg.Endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("files storage: bad endpoint: %w", err)
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	return &BlobStore{
		host:         cfg.Bucket + "." + u.Host,
		endpointHost: u.Host,
		prefix:       strings.Trim(cfg.Prefix, "/"),
		region:       region,
		access:       cfg.AccessKey,
		secret:       cfg.SecretKey,
		publicBase:   strings.TrimRight(cfg.PublicBase, "/"),
		http:         &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Enabled reports whether storage was configured.
func (b *BlobStore) Enabled() bool { return b != nil }

func (b *BlobStore) objectKey(userID, id, filename string) string {
	safe := sanitizeFilename(filename)
	parts := []string{}
	if b.prefix != "" {
		parts = append(parts, b.prefix)
	}
	parts = append(parts, "u", userID, id+"-"+safe)
	return strings.Join(parts, "/")
}

// Put uploads body and returns (objectKey, publicURL).
func (b *BlobStore) Put(ctx context.Context, key, contentType string, data []byte) (publicURL string, err error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+b.host+"/"+escapeKey(key), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	// Best-effort public ACL. Some OVH/S3 setups reject ACL and rely on bucket policy instead.
	req.Header.Set("x-amz-acl", "public-read")
	b.sign(req, hexSHA256(data))
	resp, err := b.http.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Retry without ACL if the backend rejects it.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
			req2, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+b.host+"/"+escapeKey(key), bytes.NewReader(data))
			if err != nil {
				return "", err
			}
			req2.Header.Set("Content-Type", contentType)
			b.sign(req2, hexSHA256(data))
			resp2, err := b.http.Do(req2)
			if err != nil {
				return "", err
			}
			io.Copy(io.Discard, resp2.Body)
			resp2.Body.Close()
			if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
				return b.publicURL(key), nil
			}
			return "", fmt.Errorf("s3 put %s: status %d (retry %d)", key, resp.StatusCode, resp2.StatusCode)
		}
		return "", fmt.Errorf("s3 put %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return b.publicURL(key), nil
}

// Delete removes an object (best-effort).
func (b *BlobStore) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://"+b.host+"/"+escapeKey(key), nil)
	if err != nil {
		return err
	}
	b.sign(req, emptyPayloadHash)
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 delete %s: status %d", key, resp.StatusCode)
	}
	return nil
}

func (b *BlobStore) publicURL(key string) string {
	if b.publicBase != "" {
		return b.publicBase + "/" + key
	}
	return "https://" + b.host + "/" + key
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// ── SigV4 (same style as colmena/backup/s3) ─────────────────────────────────

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (b *BlobStore) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("Host", b.host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders, canonicalHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		"", // no query
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + b.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := awsSignKey(b.secret, dateStamp, b.region, "s3")
	sig := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+b.access+"/"+scope+
			", SignedHeaders="+signedHeaders+
			", Signature="+sig)
}

func canonicalHeaders(req *http.Request) (signed, canonical string) {
	type pair struct{ k, v string }
	var pairs []pair
	for k, vv := range req.Header {
		lk := strings.ToLower(k)
		pairs = append(pairs, pair{lk, strings.Join(vv, ",")})
	}
	// Host may only be in URL
	if req.Header.Get("Host") == "" && req.URL.Host != "" {
		pairs = append(pairs, pair{"host", req.URL.Host})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
	var names []string
	var b strings.Builder
	for _, p := range pairs {
		names = append(names, p.k)
		b.WriteString(p.k)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(p.v))
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func awsSignKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}
