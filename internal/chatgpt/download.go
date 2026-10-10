package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

type AttachmentUnavailableError struct{ Reason string }

func (e *AttachmentUnavailableError) Error() string { return e.Reason }

// AttachmentReadError describes a failed read, never an upload. Callers can
// retry a temporary GET after discarding its partial file. Storage capabilities
// and raw transport errors must not enter the error text.
type AttachmentReadError struct {
	HTTPStatus int
	Retryable  bool
	cause      error
}

func (e *AttachmentReadError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("attachment storage returned HTTP %d", e.HTTPStatus)
	}
	return "attachment download is not ready or temporarily unavailable"
}
func (e *AttachmentReadError) Unwrap() error { return e.cause }

type FileData struct {
	Name   string
	MIME   string
	Data   []byte
	Width  int
	Height int
}

type FileInfo struct {
	Name string
	MIME string
	Size int64
}

// DownloadAttachment retains the upstream API, which necessarily buffers the
// whole file. New bridge code should use DownloadAttachmentTo with a temporary
// file and publish/rename that file only after a successful return.
func (c *Client) DownloadAttachment(ctx context.Context, room, fileID string) (*FileData, error) {
	var buffer bytes.Buffer
	info, err := c.DownloadAttachmentTo(ctx, room, fileID, &buffer)
	if err != nil {
		return nil, err
	}
	file := &FileData{Name: info.Name, MIME: info.MIME, Data: buffer.Bytes()}
	if config, _, err := image.DecodeConfig(bytes.NewReader(file.Data)); err == nil && config.Width > 0 && config.Height > 0 {
		file.Width, file.Height = config.Width, config.Height
	}
	return file, nil
}

// DownloadAttachmentTo copies the original bytes to dst using bounded memory.
// It never follows a storage redirect or sends account credentials to storage.
// On error dst may contain partial bytes; callers own and must discard them.
// When metadata is available, the returned info.Size records bytes written.
func (c *Client) DownloadAttachmentTo(ctx context.Context, room, fileID string, dst io.Writer) (*FileInfo, error) {
	c.mu.RLock()
	verified := c.rooms[room]
	c.mu.RUnlock()
	if !verified || fileID == "" {
		return nil, errors.New("refusing attachment from unverified Dot room or missing file identity")
	}
	if dst == nil {
		return nil, errors.New("attachment download destination is missing")
	}
	var metadata struct {
		ID     string `json:"id"`
		FileID string `json:"file_id"`
		URL    string `json:"download_url"`
		Name   string `json:"name"`
		MIME   string `json:"mime_type"`
	}
	if err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(room)+"/files/"+url.PathEscape(fileID), nil, &metadata); err != nil {
		var transport *TransportError
		if errors.As(err, &transport) {
			return nil, &AttachmentReadError{Retryable: true}
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusGone) {
			return nil, &AttachmentUnavailableError{Reason: "ChatGPT no longer provides this file"}
		}
		return nil, err
	}
	if (metadata.ID != "" && metadata.ID != fileID) || (metadata.FileID != "" && metadata.FileID != fileID) {
		return nil, errors.New("attachment metadata identity mismatch")
	}
	if metadata.URL == "" {
		return nil, &AttachmentReadError{Retryable: true}
	}
	u, err := url.Parse(metadata.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".oaiusercontent.com") {
		return nil, &AttachmentUnavailableError{Reason: "unsupported attachment storage origin"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid attachment download request")
	}
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	resp, err := c.mediaHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &AttachmentReadError{Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, &AttachmentUnavailableError{Reason: "ChatGPT attachment is no longer available"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &AttachmentReadError{HTTPStatus: resp.StatusCode,
			Retryable: resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500}
	}
	reader := contextReader{ctx: ctx, reader: resp.Body}
	prefix := make([]byte, 512)
	n, err := io.ReadFull(reader, prefix)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, downloadCopyError(ctx, err, 0)
	}
	prefix = prefix[:n]
	name := sanitizeDownloadName(metadata.Name)
	mimeType, _, err := mime.ParseMediaType(metadata.MIME)
	if err != nil || mimeType == "" || mimeType == "application/octet-stream" {
		mimeType, _, _ = mime.ParseMediaType(http.DetectContentType(prefix))
		if mimeType == "application/octet-stream" {
			if extensionType := mime.TypeByExtension(strings.ToLower(path.Ext(name))); extensionType != "" {
				mimeType, _, _ = mime.ParseMediaType(extensionType)
			}
		}
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	info := &FileInfo{Name: name, MIME: mimeType}
	info.Size, err = io.Copy(dst, contextReader{ctx: ctx, reader: io.MultiReader(bytes.NewReader(prefix), reader)})
	if err != nil {
		return info, downloadCopyError(ctx, err, info.Size)
	}
	if resp.ContentLength >= 0 && info.Size != resp.ContentLength {
		return info, fmt.Errorf("attachment download length mismatch after %d bytes", info.Size)
	}
	return info, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		return n, &AttachmentReadError{Retryable: true, cause: err}
	}
	return n, err
}

func downloadCopyError(ctx context.Context, err error, written int64) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return fmt.Errorf("attachment download interrupted after %d bytes: %w", written, err)
}

func sanitizeDownloadName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	if len(name) > 255 {
		name = name[:255]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name
}
