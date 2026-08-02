package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type PlaylistSegment struct {
	UploadResult
	Index    int     `json:"index"`
	Duration float64 `json:"duration"`
}

func buildPlaylist(init UploadResult, segments []PlaylistSegment, mediaSequence int) string {
	maxDuration := 1.0
	for _, segment := range segments {
		if segment.Duration > maxDuration {
			maxDuration = segment.Duration
		}
	}
	var out strings.Builder
	out.WriteString("#EXTM3U\n")
	out.WriteString("#EXT-X-VERSION:7\n")
	out.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	out.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	out.WriteString("#EXT-X-TARGETDURATION:")
	out.WriteString(strconv.Itoa(int(math.Ceil(maxDuration))))
	out.WriteByte('\n')
	out.WriteString("#EXT-X-MEDIA-SEQUENCE:")
	out.WriteString(strconv.Itoa(mediaSequence))
	out.WriteByte('\n')
	out.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=%q,BYTERANGE=%q\n", init.URL, init.ByteRange))
	for _, segment := range segments {
		out.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", segment.Duration))
		out.WriteString(fmt.Sprintf("#EXT-X-BYTERANGE:%s\n", segment.ByteRange))
		out.WriteString(segment.URL)
		out.WriteByte('\n')
	}
	out.WriteString("#EXT-X-ENDLIST\n")
	return out.String()
}
