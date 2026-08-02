package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse session file: %w", err)
	}
	if s.UploadURL == "" || s.Cookie == "" {
		return nil, fmt.Errorf("session file missing upload_url or cookie")
	}
	return &s, nil
}
