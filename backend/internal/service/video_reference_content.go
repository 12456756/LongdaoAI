package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	VideoOperationText  = "text-to-video"
	VideoOperationImage = "image-to-video"
	VideoOperationVideo = "video-to-video"
	VideoAutoDuration   = -1
)

func ValidVideoOperation(operation string) bool {
	switch operation {
	case VideoOperationText, VideoOperationImage, VideoOperationVideo:
		return true
	}
	return false
}

func ValidSeedanceReferenceDuration(model string, seconds int) bool {
	maxDuration := 30
	switch model {
	case "seedance-2.0", "seedance-2.0-fast", "seedance-2.0-mini":
		maxDuration = 15
	case "seedance-2.5":
		// Seedance 2.5 accepts up to 30 seconds.
	default:
		return true
	}
	return seconds == VideoAutoDuration || (seconds >= 4 && seconds <= maxDuration)
}

type videoContentURL struct {
	URL         string `json:"url,omitempty"`
	ReferenceID string `json:"reference_id,omitempty"`
}

type videoReferenceContent struct {
	Type     string           `json:"type"`
	Role     string           `json:"role,omitempty"`
	Text     string           `json:"text,omitempty"`
	ImageURL *videoContentURL `json:"image_url,omitempty"`
	VideoURL *videoContentURL `json:"video_url,omitempty"`
	AudioURL *videoContentURL `json:"audio_url,omitempty"`
}

func (item videoReferenceContent) mediaURL() *videoContentURL {
	switch item.Type {
	case "image_url":
		return item.ImageURL
	case "video_url":
		return item.VideoURL
	case "audio_url":
		return item.AudioURL
	}
	return nil
}

func parseVideoReferenceContent(raw []byte, prompt, operation string) ([]videoReferenceContent, error) {
	var items []videoReferenceContent
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&items); err != nil || len(items) == 0 {
		return nil, videoError(400, "invalid_request_error", "metadata.content must be a non-empty array of text, image_url, video_url or audio_url items")
	}
	counts := make(map[string]int)
	for i, item := range items {
		counts[item.Type]++
		if item.Type == "text" {
			if item.Role == "" && strings.TrimSpace(item.Text) != "" && item.ImageURL == nil && item.VideoURL == nil && item.AudioURL == nil {
				continue
			}
			return nil, videoError(400, "invalid_request_error", fmt.Sprintf("metadata.content[%d] must contain a non-empty text prompt", i))
		}
		media := item.mediaURL()
		if item.Type == "video_url" && (media == nil || (media.URL == "") == (media.ReferenceID == "") ||
			(media.ReferenceID != "" && !videoAssetPattern.MatchString(media.ReferenceID))) {
			return nil, videoError(400, "invalid_video_reference", "Provide either a valid reference_id or a video URL.")
		}
		if media == nil || item.Text != "" ||
			(item.Type != "image_url" && item.ImageURL != nil) ||
			(item.Type != "video_url" && item.VideoURL != nil) ||
			(item.Type != "audio_url" && item.AudioURL != nil) {
			return nil, videoError(400, "invalid_request_error", fmt.Sprintf("metadata.content[%d] must contain a URL matching its type", i))
		}
		if (item.Type != "video_url" && media.ReferenceID != "") || item.Role != "reference_"+strings.TrimSuffix(item.Type, "_url") ||
			(media.ReferenceID == "" && !validVideoReferenceURL(media.URL)) {
			return nil, videoError(400, "invalid_request_error", fmt.Sprintf("metadata.content[%d] must have the matching reference role and a public HTTP(S) or asset:// URL", i))
		}
	}
	if prompt != "" {
		items = append(items, videoReferenceContent{Type: "text", Text: prompt})
		counts["text"]++
	}
	if counts["text"] > 1 {
		return nil, videoError(400, "invalid_request_error", "A reference video request accepts at most one prompt")
	}
	actualOperation := VideoOperationImage
	if counts["video_url"] > 0 {
		actualOperation = VideoOperationVideo
	} else if counts["image_url"] == 0 {
		return nil, videoError(400, "invalid_request_error", "metadata.content requires at least one reference image or video")
	}
	if operation != "" && operation != actualOperation {
		return nil, videoError(400, "invalid_request_error", "X-Video-Operation does not match metadata.content")
	}
	return items, nil
}

func validateVideoReferenceCounts(items []videoReferenceContent, model string) error {
	imageLimit, videoLimit, audioLimit := 9, 3, 3
	if model == "seedance-2.5" {
		imageLimit, videoLimit, audioLimit = 30, 10, 10
	}
	counts := make(map[string]int)
	for _, item := range items {
		counts[item.Type]++
	}
	for _, limit := range []struct {
		kind string
		max  int
	}{{"image_url", imageLimit}, {"video_url", videoLimit}, {"audio_url", audioLimit}} {
		if counts[limit.kind] > limit.max {
			return videoError(400, "invalid_request_error", fmt.Sprintf("%s allows at most %d %s references", model, limit.max, limit.kind))
		}
	}
	return nil
}

func validVideoReferenceURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return false
		}
	}
	switch u.Scheme {
	case "asset":
		return u.Host == u.Hostname() && u.Path == "" && u.RawQuery == "" && !u.ForceQuery
	case "http", "https":
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Zone() == "" && videoPublicIP(ip)
		}
		return strings.Contains(host, ".") && !strings.HasSuffix(host, ".localhost")
	}
	return false
}
