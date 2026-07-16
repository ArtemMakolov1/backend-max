package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxVideoBytes int64 = 250 << 20

var defaultAttachmentRetryDelays = []time.Duration{0, 250 * time.Millisecond, time.Second}

// UploadAttachment streams one image or video to a freshly reserved MAX
// upload URL. It is an alias for UploadMedia kept for callers that model the
// operation in terms of message attachments.
func (c *Client) UploadAttachment(ctx context.Context, mediaType MediaType, filename string, content io.Reader) (MediaToken, error) {
	return c.UploadMedia(ctx, mediaType, filename, content)
}

// UploadMedia reserves one MAX upload URL and streams a multipart field named
// "data" to it. The source is never buffered in memory, which is important for
// video uploads. MAX permits one file per reservation.
func (c *Client) UploadMedia(ctx context.Context, mediaType MediaType, filename string, content io.Reader) (MediaToken, error) {
	label := string(mediaType)
	if !validMediaType(mediaType) {
		return MediaToken{}, fmt.Errorf("upload media: unsupported type %q", mediaType)
	}
	if ctx == nil {
		return MediaToken{}, fmt.Errorf("upload %s: nil context", label)
	}
	if strings.TrimSpace(filename) == "" {
		return MediaToken{}, fmt.Errorf("upload %s: filename is required", label)
	}
	if content == nil {
		return MediaToken{}, fmt.Errorf("upload %s: reader is required", label)
	}

	var reservation struct {
		URL   string `json:"url"`
		Token string `json:"token,omitempty"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/uploads", url.Values{"type": {string(mediaType)}}, nil, &reservation); err != nil {
		return MediaToken{}, err
	}

	uploadURL, err := validateUploadURL(reservation.URL)
	if err != nil {
		return MediaToken{}, fmt.Errorf("MAX %s upload URL: %w", label, err)
	}

	responseBody, responseStatus, responseHeaders, err := c.streamMultipartUpload(
		ctx,
		label,
		uploadURL,
		filename,
		content,
		mediaSizeLimit(mediaType),
	)
	if err != nil {
		return MediaToken{}, err
	}

	token := mediaUploadToken(mediaType, responseBody, reservation.Token, uploadURL.Query().Get("token"))
	if token == "" {
		return MediaToken{}, &Error{
			StatusCode: responseStatus,
			Code:       "invalid_upload_response",
			Message:    fmt.Sprintf("MAX %s upload response does not contain a token", label),
			RequestID:  firstHeader(responseHeaders, "X-Request-Id", "X-Request-ID", "X-Max-Request-Id"),
		}
	}

	return MediaToken{Type: mediaType, Token: token}, nil
}

func mediaSizeLimit(mediaType MediaType) int64 {
	if mediaType == MediaTypeVideo {
		return maxVideoBytes
	}
	return maxImageBytes
}

func (c *Client) streamMultipartUpload(
	ctx context.Context,
	label string,
	uploadURL *url.URL,
	filename string,
	content io.Reader,
	limit int64,
) ([]byte, int, http.Header, error) {
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), pipeReader)
	if err != nil {
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		return nil, 0, nil, fmt.Errorf("create %s upload request: %w", label, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Del("Authorization")

	streamDone := make(chan error, 1)
	go func() {
		streamErr := writeMultipartStream(multipartWriter, filename, label, content, limit)
		if streamErr != nil {
			_ = pipeWriter.CloseWithError(streamErr)
		} else {
			_ = pipeWriter.Close()
		}
		streamDone <- streamErr
	}()

	uploadClient := *c.httpClient
	callerRedirectPolicy := uploadClient.CheckRedirect
	uploadClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= maxUploadRedirects {
			return fmt.Errorf("too many %s upload redirects", label)
		}
		if _, err := validateUploadURL(next.URL.String()); err != nil {
			return fmt.Errorf("unsafe %s upload redirect: %w", label, err)
		}
		if next.Method != http.MethodPost {
			return fmt.Errorf("unsafe %s upload redirect changed method to %s", label, next.Method)
		}
		if callerRedirectPolicy != nil {
			if err := callerRedirectPolicy(next, via); err != nil {
				return err
			}
		}
		next.Header.Del("Authorization")
		// A streaming request cannot be replayed safely after a redirect.
		return fmt.Errorf("%s upload redirect cannot be replayed", label)
	}

	// #nosec G704 -- validateUploadURL requires absolute HTTPS without userinfo
	// or fragments; redirects are separately validated and never receive the bot
	// credential.
	resp, requestErr := uploadClient.Do(req)
	_ = pipeReader.Close()
	streamErr := <-streamDone
	if requestErr != nil {
		if streamErr != nil && !errors.Is(streamErr, io.ErrClosedPipe) && !errors.Is(streamErr, context.Canceled) {
			return nil, 0, nil, streamErr
		}
		return nil, 0, nil, fmt.Errorf("upload %s to MAX storage: %w", label, requestErr)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, readErr := readJSONBody(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		location, locationErr := resp.Location()
		if locationErr != nil {
			return nil, resp.StatusCode, resp.Header, fmt.Errorf("unsafe %s upload redirect: %w", label, locationErr)
		}
		if _, validationErr := validateUploadURL(location.String()); validationErr != nil {
			return nil, resp.StatusCode, resp.Header, fmt.Errorf("unsafe %s upload redirect: %w", label, validationErr)
		}
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("%s upload redirect cannot be replayed", label)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if readErr != nil {
			return nil, resp.StatusCode, resp.Header, responseReadError(resp, responseBody, readErr)
		}
		return nil, resp.StatusCode, resp.Header, apiError(resp, responseBody)
	}
	if streamErr != nil {
		return nil, resp.StatusCode, resp.Header, streamErr
	}
	if readErr != nil {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("read MAX %s upload response: %w", label, readErr)
	}
	return responseBody, resp.StatusCode, resp.Header.Clone(), nil
}

func writeMultipartStream(writer *multipart.Writer, filename, label string, content io.Reader, limit int64) error {
	part, err := writer.CreateFormFile("data", filename)
	if err != nil {
		return fmt.Errorf("create %s multipart body: %w", label, err)
	}
	written, copyErr := io.Copy(part, io.LimitReader(content, limit+1))
	if copyErr != nil {
		return fmt.Errorf("read %s: %w", label, copyErr)
	}
	if written > limit {
		return fmt.Errorf("%s is larger than %d bytes", label, limit)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish %s multipart body: %w", label, err)
	}
	return nil
}

func mediaUploadToken(mediaType MediaType, responseBody []byte, reservationToken, signedURLToken string) string {
	if mediaType == MediaTypeImage {
		return imageUploadToken(responseBody, reservationToken, signedURLToken)
	}

	// MAX's video-specific flow returns the token with the reservation and a
	// separate retval after upload. Accept a top-level upload token as a safe
	// compatibility fallback for the generic response shape documented by MAX.
	if token := strings.TrimSpace(reservationToken); token != "" {
		return token
	}
	var response struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(responseBody, &response) == nil {
		return strings.TrimSpace(response.Token)
	}
	return ""
}

func (c *Client) withAttachmentRetry(ctx context.Context, attempt func() error) error {
	delays := c.attachmentRetryDelays
	if len(delays) == 0 {
		delays = []time.Duration{0}
	}

	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		lastErr = attempt()
		if lastErr == nil || !isAttachmentNotReady(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func isAttachmentNotReady(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Code == "attachment.not.ready"
}
