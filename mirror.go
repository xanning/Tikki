package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultMirrorBase = ""

type MirrorRequest struct {
	BaseURL string `json:"base_url"`
	Start   *int   `json:"start"`
	End     *int   `json:"end"`
}

type MirrorResponse struct {
	Playlist string            `json:"playlist"`
	Init     UploadResult      `json:"init"`
	Segments []PlaylistSegment `json:"segments"`
	Elapsed  string            `json:"elapsed"`
}

type MirrorService struct {
	Uploader *TikTokClient
	HTTP     *http.Client
	mu       sync.Mutex
}

func newMirrorService(uploader *TikTokClient) *MirrorService {
	return &MirrorService{
		Uploader: uploader,
		HTTP: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (m *MirrorService) Run(ctx context.Context, request MirrorRequest) (MirrorResponse, error) {
	if !m.mu.TryLock() {
		return MirrorResponse{}, fmt.Errorf("a mirror job is already running")
	}
	defer m.mu.Unlock()
	started := time.Now()
	base := strings.TrimRight(request.BaseURL, "/")
	if base == "" {
		base = defaultMirrorBase
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return MirrorResponse{}, fmt.Errorf("base_url must be an absolute HTTP URL")
	}
	start, end := 0, 100
	if request.Start != nil {
		start = *request.Start
	}
	if request.End != nil {
		end = *request.End
	}
	if start < 0 || end < start || end-start > 1000 {
		return MirrorResponse{}, fmt.Errorf("invalid segment range")
	}
	initPayload, err := m.download(ctx, base+"/init.mp4")
	if err != nil {
		return MirrorResponse{}, fmt.Errorf("download init.mp4: %w", err)
	}
	initUpload, err := m.Uploader.Upload(ctx, initPayload, "m4s")
	if err != nil {
		return MirrorResponse{}, fmt.Errorf("upload init.mp4: %w", err)
	}
	segments := make([]PlaylistSegment, 0, end-start+1)
	for index := start; index <= end; index++ {
		payload, err := m.download(ctx, fmt.Sprintf("%s/seg/%d.m4s", base, index))
		if err != nil {
			return MirrorResponse{}, fmt.Errorf("download segment %d: %w", index, err)
		}
		upload, err := m.Uploader.Upload(ctx, payload, "m4s")
		if err != nil {
			return MirrorResponse{}, fmt.Errorf("upload segment %d: %w", index, err)
		}
		segments = append(segments, PlaylistSegment{
			UploadResult: upload,
			Index:        index,
			Duration:     probeDuration(ctx, payload),
		})
	}
	return MirrorResponse{
		Playlist: buildPlaylist(initUpload, segments, start),
		Init:     initUpload,
		Segments: segments,
		Elapsed:  time.Since(started).Round(time.Millisecond).String(),
	}, nil
}

func (m *MirrorService) download(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Tikki/1.0")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %s", res.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(res.Body, 128<<20))
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("upstream returned an empty body")
	}
	return payload, nil
}

func probeDuration(ctx context.Context, payload []byte) float64 {
	temp, err := os.CreateTemp("", "tikki-*.m4s")
	if err != nil {
		return 10
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return 10
	}
	if err := temp.Close(); err != nil {
		return 10
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", name)
	out, err := command.Output()
	if err != nil {
		return 10
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 {
		return 10
	}
	return duration
}

func decodeMirrorRequest(r *http.Request) (MirrorRequest, error) {
	var request MirrorRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := decoder.Decode(&request); err != nil && err != io.EOF {
		return MirrorRequest{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	return request, nil
}
