package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Session struct {
	UploadURL string `json:"upload_url"`
	Cookie    string `json:"cookie"`
	CSRFToken string `json:"csrf_token"`
	Referer   string `json:"referer"`
	Origin    string `json:"origin"`
	UserAgent string `json:"user_agent"`
}

func loadSession(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	text := strings.TrimLeft(string(raw), " \t\r\n")
	var s Session
	if isRawHTTPRequest(text) {
		parsed, err := parseRawHTTPRequest(text)
		if err != nil {
			return nil, fmt.Errorf("parse raw request session file: %w", err)
		}
		s = *parsed
	} else if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse session file: %w", err)
	}
	if s.UploadURL == "" || s.Cookie == "" {
		return nil, fmt.Errorf("session file missing upload_url or cookie")
	}
	return &s, nil
}

func isRawHTTPRequest(text string) bool {
	for _, method := range []string{"POST ", "GET ", "PUT "} {
		if strings.HasPrefix(text, method) {
			return true
		}
	}
	return false
}


func parseRawHTTPRequest(text string) (*Session, error) {
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	if !scanner.Scan() {
		return nil, fmt.Errorf("empty request")
	}
	requestLine := strings.TrimSpace(scanner.Text())
	parts := strings.Fields(requestLine)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed request line: %q", requestLine)
	}
	pathAndQuery := parts[1]

	headers := make(map[string]string)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		colon := strings.Index(line, ":")
		if colon == -1 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		headers[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	host := headers["host"]
	if host == "" {
		return nil, fmt.Errorf("request is missing a Host header")
	}
	scheme := "https"
	if strings.HasPrefix(pathAndQuery, "http://") || strings.HasPrefix(pathAndQuery, "https://") {
		scheme = ""
	}
	uploadURL := pathAndQuery
	if scheme != "" {
		uploadURL = scheme + "://" + host + pathAndQuery
	}

	return &Session{
		UploadURL: uploadURL,
		Cookie:    headers["cookie"],
		CSRFToken: headers["x-csrftoken"],
		Referer:   headers["referer"],
		Origin:    headers["origin"],
		UserAgent: headers["user-agent"],
	}, nil
}
