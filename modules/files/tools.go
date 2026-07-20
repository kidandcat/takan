// Package files uploads user files to S3-compatible storage and returns public URLs.
package files

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

const maxUploadBytes = 25 << 20 // 25 MiB

// Module holds storage + DB.
type Module struct {
	Store *store.Store
	Blob  *BlobStore // nil if server storage not configured
}

// Factory returns files_* tools.
func (m *Module) Factory() func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return m.tools()
	}
}

func (m *Module) tools() []mcp.RegisteredTool {
	return []mcp.RegisteredTool{
		{
			Tool: mcp.Tool{
				Name: "files_upload",
				Description: "Upload a file (or image) to public object storage and return a public URL. " +
					"Pass content_base64 (raw base64 or data URL) + filename. " +
					"MCP clients that attach images may pass the image as base64 here if they expose bytes to tools; " +
					"otherwise upload from the Takan panel (Files). Optional content_type.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename":       map[string]any{"type": "string"},
						"content_base64": map[string]any{"type": "string"},
						"content_type":   map[string]any{"type": "string"},
					},
					"required": []string{"filename", "content_base64"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if m.Blob == nil {
					return "", fmt.Errorf("file storage not configured on the server (TAKAN_FILES_*)")
				}
				name, _ := args["filename"].(string)
				b64, _ := args["content_base64"].(string)
				ct, _ := args["content_type"].(string)
				data, ct2, err := decodeBase64Payload(b64, ct)
				if err != nil {
					return "", err
				}
				sh, err := m.Upload(ctx, userID, name, ct2, data)
				if err != nil {
					return "", err
				}
				return marshal(map[string]any{
					"status": "uploaded",
					"id":     sh.ID,
					"url":    sh.PublicURL,
					"name":   sh.Filename,
					"bytes":  sh.SizeBytes,
				})
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "files_upload_url",
				Description: "Download a remote URL and re-host it on public storage. Returns a public URL.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":      map[string]any{"type": "string"},
						"filename": map[string]any{"type": "string", "description": "Optional override filename"},
					},
					"required": []string{"url"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				if m.Blob == nil {
					return "", fmt.Errorf("file storage not configured on the server (TAKAN_FILES_*)")
				}
				rawURL, _ := args["url"].(string)
				name, _ := args["filename"].(string)
				data, ct, fname, err := fetchURL(ctx, rawURL)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(name) != "" {
					fname = name
				}
				sh, err := m.Upload(ctx, userID, fname, ct, data)
				if err != nil {
					return "", err
				}
				return marshal(map[string]any{
					"status": "uploaded",
					"id":     sh.ID,
					"url":    sh.PublicURL,
					"name":   sh.Filename,
					"bytes":  sh.SizeBytes,
				})
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "files_list",
				Description: "List files uploaded by this account (id, name, url, size, created).",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				list, err := m.Store.ListFileShares(ctx, userID)
				if err != nil {
					return "", err
				}
				type row struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					URL     string `json:"url"`
					Bytes   int64  `json:"bytes"`
					Created string `json:"created"`
				}
				out := make([]row, 0, len(list))
				for _, sh := range list {
					out = append(out, row{
						ID: sh.ID, Name: sh.Filename, URL: sh.PublicURL, Bytes: sh.SizeBytes,
						Created: sh.CreatedAt.UTC().Format(time.RFC3339),
					})
				}
				return marshal(out)
			},
		},
		{
			Tool: mcp.Tool{
				Name:        "files_delete",
				Description: "Delete a previously uploaded file by id (from files_list).",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"id": map[string]any{"type": "string"}},
					"required":   []string{"id"},
				},
			},
			Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				sh, err := m.Store.GetFileShare(ctx, userID, id)
				if err != nil {
					return "", fmt.Errorf("file not found")
				}
				if m.Blob != nil {
					_ = m.Blob.Delete(ctx, sh.ObjectKey)
				}
				if err := m.Store.DeleteFileShare(ctx, userID, id); err != nil {
					return "", err
				}
				return marshal(map[string]any{"status": "deleted", "id": id})
			},
		},
	}
}

// Upload stores bytes in object storage and records a share row.
func (m *Module) Upload(ctx context.Context, userID, filename, contentType string, data []byte) (*store.FileShare, error) {
	if m.Blob == nil {
		return nil, fmt.Errorf("file storage not configured")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	if len(data) > maxUploadBytes {
		return nil, fmt.Errorf("file too large (max %d bytes)", maxUploadBytes)
	}
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = "upload.bin"
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	id := uuid.NewString()
	key := m.Blob.objectKey(userID, id, filename)
	pub, err := m.Blob.Put(ctx, key, contentType, data)
	if err != nil {
		return nil, err
	}
	sh := store.FileShare{
		ID: id, UserID: userID, ObjectKey: key, Filename: filename,
		ContentType: contentType, SizeBytes: int64(len(data)), PublicURL: pub,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.Store.CreateFileShare(ctx, sh); err != nil {
		_ = m.Blob.Delete(ctx, key)
		return nil, err
	}
	return &sh, nil
}

func decodeBase64Payload(b64, contentType string) ([]byte, string, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, "", fmt.Errorf("content_base64 required")
	}
	// data:[<mediatype>][;base64],<data>
	if strings.HasPrefix(b64, "data:") {
		rest := strings.TrimPrefix(b64, "data:")
		meta, dataPart, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, "", fmt.Errorf("invalid data URL")
		}
		if strings.Contains(meta, ";") {
			ct := strings.Split(meta, ";")[0]
			if contentType == "" && ct != "" {
				contentType = ct
			}
		}
		b64 = dataPart
	}
	// tolerate whitespace/newlines
	b64 = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, b64)
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(b64)
	}
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(b64)
	}
	if err != nil {
		return nil, "", fmt.Errorf("invalid base64: %w", err)
	}
	return data, contentType, nil
}

func fetchURL(ctx context.Context, raw string) (data []byte, contentType, filename string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", "", fmt.Errorf("url required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", "", err
	}
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("fetch status %d", resp.StatusCode)
	}
	lr := io.LimitReader(resp.Body, maxUploadBytes+1)
	data, err = io.ReadAll(lr)
	if err != nil {
		return nil, "", "", err
	}
	if len(data) > maxUploadBytes {
		return nil, "", "", fmt.Errorf("remote file too large (max %d bytes)", maxUploadBytes)
	}
	contentType = resp.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	filename = "download"
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "filename=") {
		// crude parse
		if i := strings.Index(cd, "filename="); i >= 0 {
			fn := strings.Trim(cd[i+9:], `"' `)
			if fn != "" {
				filename = fn
			}
		}
	} else {
		path := raw
		if j := strings.IndexByte(path, '?'); j >= 0 {
			path = path[:j]
		}
		if i := strings.LastIndexByte(path, '/'); i >= 0 && i+1 < len(path) {
			filename = path[i+1:]
		}
	}
	return data, contentType, filename, nil
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
