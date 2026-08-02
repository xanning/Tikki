package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
)

type TikTokClient struct {
	HTTP        *http.Client
	SessionPath string
}

type UploadResult struct {
	URL        string `json:"url"`
	Type       string `json:"type"`
	Size       int    `json:"size"`
	ByteRange  string `json:"byterange"`
	ObjectSize int    `json:"object_size"`
}

type urlCandidate struct {
	URL   string
	Score int
}

func newTikTokClient(sessionPath string) *TikTokClient {
	return &TikTokClient{
		HTTP: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		SessionPath: sessionPath,
	}
}

func (c *TikTokClient) Upload(ctx context.Context, payload []byte, kind string) (UploadResult, error) {
	session, err := loadSession(c.SessionPath)
	if err != nil {
		return UploadResult{}, err
	}
	polyglot, offset := wrapPolyglot(payload)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="Filedata"; filename="segment.png"`)
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	if err != nil {
		return UploadResult{}, fmt.Errorf("create multipart upload: %w", err)
	}
	if _, err := part.Write(polyglot); err != nil {
		return UploadResult{}, fmt.Errorf("write multipart upload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return UploadResult{}, fmt.Errorf("finish multipart upload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.UploadURL, &body)
	if err != nil {
		return UploadResult{}, fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", session.Cookie)
	req.Header.Set("Origin", session.Origin)
	req.Header.Set("Referer", session.Referer)
	req.Header.Set("User-Agent", session.UserAgent)
	if session.CSRFToken != "" {
		req.Header.Set("X-CSRFToken", session.CSRFToken)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return UploadResult{}, fmt.Errorf("upload request: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return UploadResult{}, fmt.Errorf("read upload response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return UploadResult{}, fmt.Errorf("upload returned %s: %s", res.Status, compactBody(raw))
	}
	cdnURL, err := extractUploadURL(raw)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		URL:        cdnURL,
		Type:       kind,
		Size:       len(payload),
		ByteRange:  fmt.Sprintf("%d@%d", len(payload), offset),
		ObjectSize: len(polyglot),
	}, nil
}

func compactBody(raw []byte) string {
	value := strings.TrimSpace(string(raw))
	if len(value) > 500 {
		value = value[:500] + "..."
	}
	return value
}

func extractUploadURL(raw []byte) (string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("parse upload response: %w: %s", err, compactBody(raw))
	}
	candidates := make([]urlCandidate, 0)
	collectURLs(value, "", &candidates)
	if len(candidates) == 0 {
		return "", fmt.Errorf("upload response contained no CDN URL: %s", compactBody(raw))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates[0].URL, nil
}

func collectURLs(value any, key string, out *[]urlCandidate) {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			collectURLs(child, strings.ToLower(childKey), out)
		}
	case []any:
		for _, child := range typed {
			collectURLs(child, key, out)
		}
	case string:
		parsed, err := url.Parse(typed)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return
		}
		score := 1
		if strings.Contains(key, "url") || strings.Contains(key, "uri") {
			score += 10
		}
		host := strings.ToLower(parsed.Host)
		if strings.Contains(host, "tiktok") || strings.Contains(host, "byte") || strings.Contains(host, "p16") || strings.Contains(host, "p19") || strings.Contains(host, "cdn") {
			score += 20
		}
		path := strings.ToLower(parsed.Path)
		if strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg") || strings.Contains(path, "image") {
			score += 5
		}
		*out = append(*out, urlCandidate{URL: typed, Score: score})
	}
}
