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
	sessionPath := flag.String("session", "session.json", "session file")
	flag.Parse()
	if err := initPolyglot(); err != nil {
		log.Fatal(err)
	}
	uploader := newTikTokClient(*sessionPath)
	app := &Server{uploader: uploader, mirror: newMirrorService(uploader)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.index)
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("POST /upload/segment/", app.uploadSegment)
	mux.HandleFunc("POST /mirror/run", app.runMirror)
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
		"endpoints":  []string{"POST /upload/segment/", "POST /mirror/run", "GET /health"},
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

func (s *Server) runMirror(w http.ResponseWriter, r *http.Request) {
	request, err := decodeMirrorRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.mirror.Run(r.Context(), request)
	if err != nil {
		status := http.StatusBadGateway
		if err.Error() == "a mirror job is already running" {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
