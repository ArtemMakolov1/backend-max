package maxclient

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
)

var videoTokenPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// VideoURLs is the documented nullable/optional set of playback sources.
// Empty or unsafe URLs are omitted; callers must not infer a processing state.
type VideoURLs struct {
	MP41080 string `json:"mp4_1080,omitempty"`
	MP4720  string `json:"mp4_720,omitempty"`
	MP4480  string `json:"mp4_480,omitempty"`
	MP4360  string `json:"mp4_360,omitempty"`
	MP4240  string `json:"mp4_240,omitempty"`
	MP4144  string `json:"mp4_144,omitempty"`
	HLS     string `json:"hls,omitempty"`
}

func (urls VideoURLs) HasSource() bool {
	return urls.MP41080 != "" || urls.MP4720 != "" || urls.MP4480 != "" ||
		urls.MP4360 != "" || urls.MP4240 != "" || urls.MP4144 != "" || urls.HLS != ""
}

// SafeVideoURLs also validates results from alternative MAXClient implementations.
func SafeVideoURLs(urls *VideoURLs) *VideoURLs {
	if urls == nil {
		return nil
	}
	result := VideoURLs{
		MP41080: SafeAssetURL(urls.MP41080), MP4720: SafeAssetURL(urls.MP4720),
		MP4480: SafeAssetURL(urls.MP4480), MP4360: SafeAssetURL(urls.MP4360),
		MP4240: SafeAssetURL(urls.MP4240), MP4144: SafeAssetURL(urls.MP4144),
		HLS: SafeAssetURL(urls.HLS),
	}
	if !result.HasSource() {
		return nil
	}
	return &result
}

type VideoInfo struct {
	Token        string     `json:"-"`
	URLs         *VideoURLs `json:"urls"`
	ThumbnailURL string     `json:"thumbnail_url,omitempty"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	DurationMS   int64      `json:"duration_ms"`
}

type videoReadError struct{ cause error }

func (e videoReadError) Error() string { return "MAX video metadata request failed" }
func (e videoReadError) Unwrap() error { return e.cause }

// GetVideo reads metadata only. Tokens stay server-side, and URLs are never
// fetched here. MAX returns urls:null when playback is unavailable.
func (c *Client) GetVideo(ctx context.Context, token string) (VideoInfo, error) {
	if len(token) > 4096 || !videoTokenPattern.MatchString(token) {
		return VideoInfo{}, errors.New("MAX video token must match the documented token pattern")
	}
	var response struct {
		Token     string     `json:"token"`
		URLs      *VideoURLs `json:"urls"`
		Thumbnail *struct {
			URL string `json:"url"`
		} `json:"thumbnail"`
		Width    *int   `json:"width"`
		Height   *int   `json:"height"`
		Duration *int64 `json:"duration"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/videos/"+token, nil, nil, &response); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) {
			// Provider error bodies can echo the video token. Keep status/retry
			// semantics without copying those bodies into logs or API responses.
			return VideoInfo{}, &Error{StatusCode: apiErr.StatusCode, Code: "video.metadata_unavailable",
				Message: "MAX video metadata request failed", RetryAfter: apiErr.RetryAfter}
		}
		return VideoInfo{}, videoReadError{cause: err}
	}
	if response.Token != token || response.Width == nil || response.Height == nil || response.Duration == nil ||
		*response.Width < 0 || *response.Height < 0 || *response.Duration < 0 || *response.Duration > math.MaxInt64/1000 {
		return VideoInfo{}, errors.New("MAX video metadata response is incomplete or inconsistent")
	}
	result := VideoInfo{Token: response.Token, URLs: SafeVideoURLs(response.URLs),
		Width: *response.Width, Height: *response.Height, DurationMS: *response.Duration * 1000}
	if response.Thumbnail != nil {
		result.ThumbnailURL = SafeAssetURL(response.Thumbnail.URL)
	}
	return result, nil
}
