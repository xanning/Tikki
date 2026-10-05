package main

import (
	"bufio"
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
const playlistStorageDir = "/var/lib/tikki/playlists"

type MirrorRequest struct {
	PlaylistURL string `json:"playlist_url"`
	BaseURL     string `json:"base_url"`
	Start       *int   `json:"start"`
	End         *int   `json:"end"`
	Concurrency *int   `json:"concurrency"`
}

type MirrorResponse struct {
	Playlist       string            `json:"playlist"`
	VideoPlaylist  string            `json:"video_playlist"`
	AudioPlaylist  string            `json:"audio_playlist"`
	PlaylistURL    string            `json:"playlist_url"`
	Init           UploadResult      `json:"init"`
	AudioInit      UploadResult      `json:"audio_init"`
	Segments       []PlaylistSegment `json:"segments"`
	AudioSegments  []PlaylistSegment `json:"audio_segments"`
	Elapsed        string            `json:"elapsed"`
}

type Job struct {
	ID           string
	Status       string
	Request      MirrorRequest
	Result       MirrorResponse
	Error        string
	Started      time.Time
	Finished     time.Time
	Uploaded     int
	Total        int
	resultMu     sync.Mutex
}

type MirrorService struct {
	Uploader    *TikTokClient
	HTTP        *http.Client
	mu          sync.Mutex
	jobs        map[string]*Job
	jobsMu      sync.RWMutex
	Concurrency int
	BaseURL     string
}

func newMirrorService(uploader *TikTokClient, concurrency int) *MirrorService {
	if concurrency < 1 {
		concurrency = 8
	}
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
		jobs:        make(map[string]*Job),
		Concurrency: concurrency,
	}
}

func (m *MirrorService) Submit(ctx context.Context, request MirrorRequest) (string, error) {
	jobID := fmt.Sprintf("%d", time.Now().UnixNano())
	job := &Job{
		ID:      jobID,
		Status:  "queued",
		Request: request,
		Started: time.Now(),
	}
	m.jobsMu.Lock()
	m.jobs[jobID] = job
	m.jobsMu.Unlock()

	concurrency := m.Concurrency
	if request.Concurrency != nil && *request.Concurrency > 0 {
		concurrency = *request.Concurrency
	}
	go m.runJob(jobID, request, concurrency)
	return jobID, nil
}

func (m *MirrorService) GetStatus(jobID string) *Job {
	m.jobsMu.RLock()
	defer m.jobsMu.RUnlock()
	return m.jobs[jobID]
}

func (m *MirrorService) runJob(jobID string, request MirrorRequest, concurrency int) {
	m.jobsMu.Lock()
	job := m.jobs[jobID]
	job.Status = "running"
	m.jobsMu.Unlock()

	result, err := m.runWithProgress(context.Background(), jobID, request, concurrency)

	m.jobsMu.Lock()
	job = m.jobs[jobID]
	job.Finished = time.Now()
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
		job.Result = result
	}
	m.jobsMu.Unlock()
}

func (m *MirrorService) runWithProgress(ctx context.Context, jobID string, request MirrorRequest, concurrency int) (MirrorResponse, error) {
	if request.PlaylistURL != "" {
		return m.mirrorFromM3U8WithProgress(ctx, jobID, request, concurrency)
	}
	return m.Run(ctx, request)
}

func (m *MirrorService) mirrorFromM3U8WithProgress(ctx context.Context, jobID string, request MirrorRequest, concurrency int) (MirrorResponse, error) {
	started := time.Now()
	logFile, _ := os.OpenFile("/var/log/tikki/mirror-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	defer logFile.Close()
	log := func(s string) { fmt.Fprintf(logFile, "[%s] %s\n", time.Now().Format("15:04:05"), s) }
	var audioVariant string

	log("START: mirrorFromM3U8WithProgress")
	masterPlaylist, err := m.download(ctx, request.PlaylistURL)
	if err != nil {
		log(fmt.Sprintf("ERROR download master: %v", err))
		return MirrorResponse{}, fmt.Errorf("download master playlist: %w", err)
	}
	log(fmt.Sprintf("OK: master playlist %d bytes", len(masterPlaylist)))

	variant1080pAAC := m.find1080pAACVariant(string(masterPlaylist))
	if variant1080pAAC == "" {
		log("ERROR: no 1080p AAC variant found")
		return MirrorResponse{}, fmt.Errorf("no 1080p AAC variant found in master playlist")
	}
	log(fmt.Sprintf("OK: found variant %s", variant1080pAAC))

	log("Downloading video playlist...")
	videoPlaylist, err := m.download(ctx, variant1080pAAC)
	if err != nil {
		log(fmt.Sprintf("ERROR download video: %v", err))
		return MirrorResponse{}, fmt.Errorf("download video playlist: %w", err)
	}
	log(fmt.Sprintf("OK: video playlist %d bytes", len(videoPlaylist)))

	log("Parsing segments...")
	segments, err := m.parseHLSPlaylist(ctx, string(videoPlaylist))
	if err != nil {
		log(fmt.Sprintf("ERROR parse: %v", err))
		return MirrorResponse{}, fmt.Errorf("parse video playlist: %w", err)
	}

	if len(segments) == 0 {
		log("ERROR: no segments found")
		return MirrorResponse{}, fmt.Errorf("no segments found in video playlist")
	}
	log(fmt.Sprintf("OK: found %d video segments, processing...", len(segments)))

	audioVariant = m.findAudioVariant(string(masterPlaylist))
	audioSegCount := 0
	if audioVariant != "" {
		log("Downloading audio playlist...")
		audioPlaylist, err := m.download(ctx, audioVariant)
		if err != nil {
			log(fmt.Sprintf("WARNING: failed to download audio: %v", err))
		} else {
			audioSegs, _ := m.parseHLSPlaylist(ctx, string(audioPlaylist))
			audioSegCount = len(audioSegs)
			log(fmt.Sprintf("OK: found %d audio segments", audioSegCount))
		}
	}

	m.jobsMu.Lock()
	m.jobs[jobID].Total = len(segments) + audioSegCount
	m.jobsMu.Unlock()

	initURL := m.extractInitSegmentURL(string(videoPlaylist))
	var videoInit UploadResult
	if initURL != "" {
		log("Downloading video init segment...")
		initPayload, err := m.download(ctx, initURL)
		if err != nil {
			log(fmt.Sprintf("WARNING: failed to download init: %v", err))
		} else {
			log(fmt.Sprintf("Uploading video init segment %d bytes...", len(initPayload)))
			initUpload, err := m.Uploader.Upload(ctx, initPayload, "m4s")
			if err != nil {
				log(fmt.Sprintf("WARNING: failed to upload init: %v", err))
			} else {
				log("Video init uploaded OK")
				videoInit = initUpload
			}
		}
	}

	playlistSegments := make([]PlaylistSegment, len(segments))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.Result.Init = videoInit
	}
	m.jobsMu.Unlock()

	for i, seg := range segments {
		wg.Add(1)
		go func(idx int, url string, duration float64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			log(fmt.Sprintf("Segment %d/%d: downloading...", idx+1, len(segments)))
			payload, err := m.download(ctx, url)
			if err != nil {
				log(fmt.Sprintf("ERROR download segment %d: %v", idx, err))
				return
			}
			log(fmt.Sprintf("Segment %d: uploading %d bytes...", idx, len(payload)))
			upload, err := m.Uploader.Upload(ctx, payload, "m4s")
			if err != nil {
				log(fmt.Sprintf("ERROR upload segment %d: %v", idx, err))
				return
			}
			log(fmt.Sprintf("Segment %d: OK - %s", idx, upload.URL))
			playlistSegments[idx] = PlaylistSegment{
				UploadResult: upload,
				Index:        idx,
				Duration:     duration,
			}

			m.jobsMu.Lock()
			if job, ok := m.jobs[jobID]; ok {
				job.Uploaded++
				job.Result.Segments = playlistSegments
			}
			m.jobsMu.Unlock()
		}(i, seg.URL, seg.Duration)
	}

	wg.Wait()

	audioPlaylistSegments := []PlaylistSegment{}
	var audioInit UploadResult
	audioVariant = m.findAudioVariant(string(masterPlaylist))
	if audioVariant != "" {
		log("Downloading audio playlist...")
		audioPlaylist, err := m.download(ctx, audioVariant)
		if err != nil {
			log(fmt.Sprintf("WARNING: failed to download audio: %v", err))
		} else {
			log(fmt.Sprintf("OK: audio playlist %d bytes", len(audioPlaylist)))
			audioInitURL := m.extractInitSegmentURL(string(audioPlaylist))
			if audioInitURL != "" {
				log("Downloading audio init segment...")
				audioInitPayload, err := m.download(ctx, audioInitURL)
				if err != nil {
					log(fmt.Sprintf("WARNING: failed to download audio init: %v", err))
				} else {
					log(fmt.Sprintf("Uploading audio init segment %d bytes...", len(audioInitPayload)))
					audioInitUpload, err := m.Uploader.Upload(ctx, audioInitPayload, "m4s")
					if err != nil {
						log(fmt.Sprintf("WARNING: failed to upload audio init: %v", err))
					} else {
						log("Audio init uploaded OK")
						audioInit = audioInitUpload
					}
				}
			}

			audioSegs, err := m.parseHLSPlaylist(ctx, string(audioPlaylist))
			if err == nil && len(audioSegs) > 0 {
				log(fmt.Sprintf("Found %d audio segments", len(audioSegs)))
				audioPlaylistSegments = make([]PlaylistSegment, len(audioSegs))
				for i, seg := range audioSegs {
					wg.Add(1)
					go func(idx int, url string, duration float64) {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()

						log(fmt.Sprintf("Audio segment %d/%d: downloading...", idx+1, len(audioSegs)))
						payload, err := m.download(ctx, url)
						if err != nil {
							log(fmt.Sprintf("ERROR download audio segment %d: %v", idx, err))
							return
						}
						log(fmt.Sprintf("Audio segment %d: uploading %d bytes...", idx, len(payload)))
						upload, err := m.Uploader.Upload(ctx, payload, "m4s")
						if err != nil {
							log(fmt.Sprintf("ERROR upload audio segment %d: %v", idx, err))
							return
						}
						log(fmt.Sprintf("Audio segment %d: OK", idx))
						audioPlaylistSegments[idx] = PlaylistSegment{
							UploadResult: upload,
							Index:        idx,
							Duration:     duration,
						}

						m.jobsMu.Lock()
						if job, ok := m.jobs[jobID]; ok {
							job.Uploaded++
							job.Result.AudioSegments = audioPlaylistSegments
						}
						m.jobsMu.Unlock()
					}(i, seg.URL, seg.Duration)
				}
				wg.Wait()
			}
		}
	}

	videoPlaylistText := buildPlaylist(videoInit, playlistSegments, 0)
	audioPlaylistText := ""
	masterAudioRef := ""
	if len(audioPlaylistSegments) > 0 {
		audioPlaylistText = buildPlaylist(audioInit, audioPlaylistSegments, 0)
		masterAudioRef = "audio.m3u8"
	}
	realCodecs := m.findVideoVariantCodecs(string(masterPlaylist))
	masterPlaylistText := buildMasterPlaylist("video.m3u8", masterAudioRef, realCodecs, estimateBandwidth(playlistSegments))

	playlistURL, err := m.savePlaylistsToDisk(jobID, masterPlaylistText, videoPlaylistText, audioPlaylistText)
	if err != nil {
		log(fmt.Sprintf("WARNING: failed to save playlists to disk: %v", err))
	} else {
		log(fmt.Sprintf("Saved playlists to disk, URL: %s", playlistURL))
	}

	result := MirrorResponse{
		Playlist:      masterPlaylistText,
		VideoPlaylist: videoPlaylistText,
		AudioPlaylist: audioPlaylistText,
		PlaylistURL:   playlistURL,
		Init:          videoInit,
		AudioInit:     audioInit,
		Segments:      playlistSegments,
		AudioSegments: audioPlaylistSegments,
		Elapsed:       time.Since(started).Round(time.Millisecond).String(),
	}

	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.Result = result
	}
	m.jobsMu.Unlock()

	return result, nil
}

func (m *MirrorService) savePlaylistsToDisk(jobID, master, video, audio string) (string, error) {
	dir := playlistStorageDir + "/" + jobID
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dir+"/master.m3u8", []byte(master), 0644); err != nil {
		return "", err
	}
	if err := os.WriteFile(dir+"/video.m3u8", []byte(video), 0644); err != nil {
		return "", err
	}
	if audio != "" {
		if err := os.WriteFile(dir+"/audio.m3u8", []byte(audio), 0644); err != nil {
			return "", err
		}
	}
	base := strings.TrimRight(m.BaseURL, "/")
	return fmt.Sprintf("%s/playlists/%s/master.m3u8", base, jobID), nil
}

func estimateBandwidth(segments []PlaylistSegment) int {
	totalBytes := 0
	totalDuration := 0.0
	for _, s := range segments {
		totalBytes += s.Size
		totalDuration += s.Duration
	}
	if totalDuration <= 0 {
		return 3000000
	}
	bw := int(float64(totalBytes) * 8 / totalDuration)
	if bw <= 0 {
		return 3000000
	}
	return bw
}

func (m *MirrorService) Run(ctx context.Context, request MirrorRequest) (MirrorResponse, error) {
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

func (m *MirrorService) find1080pAACVariant(masterPlaylist string) string {
	lines := strings.Split(masterPlaylist, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "EXT-X-STREAM-INF") && strings.Contains(line, "1920x1080") && strings.Contains(line, "audio_aac") {
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(nextLine, "https://") || strings.HasPrefix(nextLine, "http://") {
					return nextLine
				}
			}
		}
	}
	return ""
}

func extractQuotedAttr(line, attr string) string {
	marker := attr + "=\""
	start := strings.Index(line, marker)
	if start == -1 {
		return ""
	}
	start += len(marker)
	end := strings.Index(line[start:], "\"")
	if end == -1 {
		return ""
	}
	return line[start : start+end]
}

func (m *MirrorService) findVideoVariantCodecs(masterPlaylist string) string {
	lines := strings.Split(masterPlaylist, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "EXT-X-STREAM-INF") && strings.Contains(line, "1920x1080") && strings.Contains(line, "mp4a") {
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(nextLine, "https://") || strings.HasPrefix(nextLine, "http://") {
					return extractQuotedAttr(line, "CODECS")
				}
			}
		}
	}
	return ""
}

func (m *MirrorService) findVideoVariantAudioGroup(masterPlaylist string) string {
	lines := strings.Split(masterPlaylist, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "EXT-X-STREAM-INF") && strings.Contains(line, "1920x1080") && strings.Contains(line, "mp4a") {
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(nextLine, "https://") || strings.HasPrefix(nextLine, "http://") {
					return extractQuotedAttr(line, "AUDIO")
				}
			}
		}
	}
	return ""
}

// findAudioVariant locates the EXT-X-MEDIA:TYPE=AUDIO line whose GROUP-ID
// matches the AUDIO attribute referenced by the chosen video variant, since
// EXT-X-STREAM-INF lines only ever describe video renditions - real
// alternate audio tracks are declared as separate EXT-X-MEDIA entries.
func (m *MirrorService) findAudioVariant(masterPlaylist string) string {
	groupID := m.findVideoVariantAudioGroup(masterPlaylist)
	if groupID == "" {
		return ""
	}
	lines := strings.Split(masterPlaylist, "\n")
	fallback := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "EXT-X-MEDIA") || !strings.Contains(line, "TYPE=AUDIO") {
			continue
		}
		if extractQuotedAttr(line, "GROUP-ID") != groupID {
			continue
		}
		uri := extractQuotedAttr(line, "URI")
		if uri == "" {
			continue
		}
		if fallback == "" {
			fallback = uri
		}
		if strings.Contains(line, "DEFAULT=YES") {
			return uri
		}
	}
	return fallback
}

type sourceSegment struct {
	URL      string
	Duration float64
}

// parseHLSPlaylist reads each segment's real #EXTINF duration from the
// source playlist rather than re-deriving it later with ffprobe on an
// isolated fragment - ffprobe cannot reliably read duration from a bare
// moof/mdat with no moov/init context, and was silently falling back to a
// hardcoded 10s for every single segment (video AND audio), causing a
// severe mismatch between the generated playlist's declared durations and
// the real embedded media timestamps (worse for audio, whose real ~6.5s
// segments were all being declared as 10.000s) - this is what caused the
// MSE "bufferAppendNoProgress"/"bufferSeekOverHole" errors during playback.
func (m *MirrorService) parseHLSPlaylist(ctx context.Context, playlist string) ([]sourceSegment, error) {
	var segments []sourceSegment
	lastDuration := 10.0
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#EXTINF:") {
			value := strings.TrimPrefix(line, "#EXTINF:")
			value = strings.SplitN(value, ",", 2)[0]
			value = strings.TrimSpace(value)
			if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
				lastDuration = parsed
			}
			continue
		}
		if strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			if !strings.Contains(line, ".m3u8") {
				segments = append(segments, sourceSegment{URL: line, Duration: lastDuration})
			}
		}
	}
	return segments, scanner.Err()
}

func (m *MirrorService) extractInitSegmentURL(playlist string) string {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "EXT-X-MAP") && strings.Contains(line, "URI=") {
			start := strings.Index(line, "URI=\"")
			if start == -1 {
				start = strings.Index(line, "URI=")
				if start == -1 {
					continue
				}
				start += 4
				end := strings.IndexAny(line[start:], " \t\n\"")
				if end == -1 {
					return strings.TrimSpace(line[start:])
				}
				return strings.TrimSpace(line[start : start+end])
			}
			start += 5
			end := strings.Index(line[start:], "\"")
			if end == -1 {
				continue
			}
			return line[start : start+end]
		}
	}
	return ""
}

func buildMasterPlaylist(videoPlaylistURL, audioPlaylistURL, codecs string, bandwidth int) string {
	var out strings.Builder
	out.WriteString("#EXTM3U\n")
	out.WriteString("#EXT-X-VERSION:7\n")
	out.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	hasAudio := audioPlaylistURL != ""
	if hasAudio {
		out.WriteString("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"AAC\",DEFAULT=YES,AUTOSELECT=YES,URI=\"")
		out.WriteString(audioPlaylistURL)
		out.WriteString("\"\n")
	}

	if codecs == "" {
		codecs = "avc1.4d4028,mp4a.40.2"
	}
	out.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=")
	out.WriteString(strconv.Itoa(bandwidth))
	out.WriteString(",CODECS=\"")
	out.WriteString(codecs)
	out.WriteString("\"")
	if hasAudio {
		out.WriteString(",AUDIO=\"audio\"")
	}
	out.WriteByte('\n')
	out.WriteString(videoPlaylistURL)
	out.WriteByte('\n')
	return out.String()
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
