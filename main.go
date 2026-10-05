package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

const maxSegmentSize = 128 << 20

type Server struct {
	uploader *TikTokClient
	mirror   *MirrorService
}

func main() {
	address := flag.String("addr", "127.0.0.1:8787", "listen address")
	sessionPath := flag.String("session", "session.txt", "session file (raw HTTP request dump or legacy JSON)")
	concurrency := flag.Int("concurrency", 8, "concurrent segment uploads")
	flag.Parse()
	if err := initPolyglot(); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(playlistStorageDir, 0755); err != nil {
		log.Fatalf("failed to create playlist storage dir: %v", err)
	}
	if err := os.MkdirAll("/var/log/tikki", 0755); err != nil {
		log.Fatalf("failed to create log dir: %v", err)
	}
	uploader := newTikTokClient(*sessionPath)
	app := &Server{uploader: uploader, mirror: newMirrorService(uploader, *concurrency)}
	app.mirror.BaseURL = "http://" + *address
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.index)
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("POST /upload/segment/", app.uploadSegment)
	mux.HandleFunc("POST /submit/mirror", app.submitMirror)
	mux.HandleFunc("GET /status/{jobID}", app.getStatus)
	mux.HandleFunc("GET /playlists/{jobID}/{file}", app.servePlaylist)
	server := &http.Server{
		Addr:              *address,
		Handler:           requestLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       3 * time.Minute,
		WriteTimeout:      3 * time.Hour,
		IdleTimeout:       90 * time.Second,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("Tikki listening on http://%s with PNG payload offset %d", *address, len(canvasHeader))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       "Tikki",
		"status":     "ready",
		"png_offset": len(canvasHeader),
		"endpoints":  []string{"POST /submit/mirror", "GET /status/{jobID}", "POST /upload/segment/", "GET /health"},
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	_, err := loadSession(s.uploader.SessionPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"session_ready":   err == nil,
		"png_offset":      len(canvasHeader),
		"session_message": errorString(err),
	})
}

func (s *Server) uploadSegment(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxSegmentSize)
	payload, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "segment body exceeds 128 MiB")
		return
	}
	if len(payload) == 0 {
		writeError(w, http.StatusBadRequest, "segment body is empty")
		return
	}
	result, err := s.uploader.Upload(r.Context(), payload, segmentKind(payload))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) submitMirror(w http.ResponseWriter, r *http.Request) {
	request, err := decodeMirrorRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	jobID, err := s.mirror.Submit(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

var jobIDPattern = regexp.MustCompile(`^[0-9]+$`)
var allowedPlaylistFiles = map[string]bool{"master.m3u8": true, "video.m3u8": true, "audio.m3u8": true}

func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobID")
	file := r.PathValue("file")
	if !jobIDPattern.MatchString(jobID) || !allowedPlaylistFiles[file] {
		writeError(w, http.StatusBadRequest, "invalid playlist path")
		return
	}
	path := playlistStorageDir + "/" + jobID + "/" + file
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "playlist not found")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobID")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, "missing job_id parameter")
		return
	}
	job := s.mirror.GetStatus(jobID)
	if job == nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func errorString(err error) string {
	if err == nil {
		return "ready"
	}
	return err.Error()
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}
