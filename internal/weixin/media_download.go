package weixin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

var ErrMediaRead = errors.New("Weixin media download could not be completed")

// MediaReadError contains stable diagnostics, never a signed URL or response
// body. Retryable describes a safe GET retry; it never authorizes a POST retry.
type MediaReadError struct {
	Reason        string
	HTTPStatus    int
	BytesRead     int64
	ExpectedBytes int64
	Retryable     bool
	cause         error
}

func (e *MediaReadError) Error() string {
	return fmt.Sprintf("Weixin media read failed: reason=%s http=%d bytes=%d expected=%d retryable=%t", e.Reason, e.HTTPStatus, e.BytesRead, e.ExpectedBytes, e.Retryable)
}

func (e *MediaReadError) Is(target error) bool { return target == ErrMediaRead || target == ErrAPI }
func (e *MediaReadError) Unwrap() error        { return e.cause }

func mediaReadFailure(ctx context.Context, reason string, status int, read, expected int64, retryable bool) error {
	return &MediaReadError{Reason: reason, HTTPStatus: status, BytesRead: read, ExpectedBytes: expected, Retryable: retryable, cause: ctx.Err()}
}

// This is a per-request transport window, not an attachment size limit. The
// complete attachment may contain any number of windows and uses disk storage.
const mediaDownloadRangeBytes int64 = 8 * 1024 * 1024

type mediaDownloadWriter struct {
	destination io.Writer
	failed      bool
}

func (w *mediaDownloadWriter) Write(p []byte) (int, error) {
	n, err := w.destination.Write(p)
	w.failed = w.failed || err != nil || n != len(p)
	return n, err
}

// downloadMediaOriginal stores the complete referenced representation before
// decryption. A resumed response must describe the precise requested range of
// the same total length. Without a strong ETag, an original MD5 is required to
// verify the assembled entity after decryption; length alone is insufficient.
func (c *Client) downloadMediaOriginal(ctx context.Context, target *url.URL, opts MediaOptions, hasOriginalMD5 bool) (*os.File, int64, error) {
	output, err := privateMediaTemp(opts.TempDir, "weixin-download-*")
	if err != nil {
		return nil, 0, err
	}
	completed := false
	defer func() {
		if !completed {
			output.Close()
			os.Remove(output.Name())
		}
	}()
	var total int64 = -1
	var downloaded int64
	var etag string
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return nil, downloaded, mediaReadFailure(ctx, "cancelled", 0, downloaded, total, false)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, downloaded, ErrInvalidRequest
		}
		req.Header.Set("Accept-Encoding", "identity")
		var end int64
		if first && hasOriginalMD5 {
			// A known plaintext MD5 permits bounded requests from byte zero,
			// avoiding a slow full-response interruption before first resume.
			end = mediaDownloadRangeBytes - 1
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", end))
		} else if !first {
			end = total - 1
			if total-downloaded > mediaDownloadRangeBytes {
				end = downloaded + mediaDownloadRangeBytes - 1
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", downloaded, end))
			if etag != "" {
				req.Header.Set("If-Range", etag)
			}
		}
		response, err := c.mediaHTTP().Do(req)
		if err != nil {
			if errors.Is(err, ErrUnsafeURL) {
				return nil, downloaded, ErrUnsafeURL
			}
			return nil, downloaded, mediaReadFailure(ctx, "transport", 0, downloaded, total, ctx.Err() == nil)
		}
		status := response.StatusCode
		initialPartial := first && hasOriginalMD5 && status == http.StatusPartialContent
		if first && status != http.StatusOK && !initialPartial || !first && status != http.StatusPartialContent {
			response.Body.Close()
			if !first && status == http.StatusOK {
				if etag != "" {
					return nil, downloaded, ErrMediaIntegrity
				}
				return nil, downloaded, mediaReadFailure(ctx, "range-unsupported", status, downloaded, total, false)
			}
			return nil, downloaded, mediaReadFailure(ctx, "http", status, downloaded, total, status == 408 || status == 429 || status >= 500)
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") || response.Uncompressed {
			response.Body.Close()
			return nil, downloaded, ErrMediaIntegrity
		}
		expected := response.ContentLength
		if first {
			if expected < -1 {
				response.Body.Close()
				return nil, downloaded, ErrMediaIntegrity
			}
			if initialPartial {
				start, finish, length, ok := parseMediaContentRange(response.Header.Get("Content-Range"))
				if length <= end {
					end = length - 1
				}
				if !ok || start != 0 || finish != end || expected >= 0 && expected != finish+1 {
					response.Body.Close()
					return nil, downloaded, ErrMediaIntegrity
				}
				total, expected = length, finish+1
			} else {
				// Some CDNs ignore Range. A complete HTTP 200 still works and
				// can later resume under the same validation rules.
				if response.Header.Get("Content-Range") != "" {
					response.Body.Close()
					return nil, downloaded, ErrMediaIntegrity
				}
				total = expected
			}
			if value := response.Header.Get("ETag"); len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && !strings.ContainsAny(value, "\r\n") {
				etag = value
			}
		} else {
			start, finish, length, ok := parseMediaContentRange(response.Header.Get("Content-Range"))
			if !ok || start != downloaded || finish != end || length != total || expected >= 0 && expected != end-downloaded+1 {
				response.Body.Close()
				return nil, downloaded, ErrMediaIntegrity
			}
			if etag != "" && response.Header.Get("ETag") != etag {
				response.Body.Close()
				return nil, downloaded, ErrMediaIntegrity
			}
			expected = end - downloaded + 1
		}
		reader := &mediaContextReader{ctx: ctx, source: response.Body}
		// Read one extra byte only to detect an invalid overlong response. This
		// window bounds an HTTP representation, never the entire attachment.
		var source io.Reader = reader
		if expected >= 0 && expected < math.MaxInt64 {
			source = io.LimitReader(reader, expected+1)
		}
		writer := &mediaDownloadWriter{destination: output}
		count, readErr := io.CopyBuffer(writer, source, make([]byte, 32768))
		response.Body.Close()
		if count > math.MaxInt64-downloaded {
			return nil, downloaded, ErrMediaIntegrity
		}
		downloaded += count
		if err := ctx.Err(); err != nil {
			return nil, downloaded, mediaReadFailure(ctx, "cancelled", status, downloaded, total, false)
		}
		if expected >= 0 && count > expected {
			return nil, downloaded, ErrMediaIntegrity
		}
		if writer.failed {
			return nil, downloaded, mediaReadFailure(ctx, "local-write", status, downloaded, total, false)
		}
		if total < 0 {
			if readErr != nil {
				return nil, downloaded, mediaReadFailure(ctx, "read-interrupted", status, downloaded, total, true)
			}
			break
		}
		if downloaded == total {
			break // final padding, plaintext size and MD5 are still verified
		}
		if count == 0 {
			return nil, downloaded, mediaReadFailure(ctx, "no-progress", status, downloaded, total, true)
		}
		if !hasOriginalMD5 && etag == "" {
			return nil, downloaded, mediaReadFailure(ctx, "resume-identity-unavailable", status, downloaded, total, true)
		}
		// Short-read transport failures with progress can safely resume GET.
		// A complete range may also be followed by the next bounded range.
		first = false
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, downloaded, mediaReadFailure(ctx, "local-seek", 0, downloaded, total, false)
	}
	completed = true
	return output, downloaded, nil
}

func parseMediaContentRange(value string) (start, end, total int64, ok bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, 0, false
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}
	var err error
	if start, err = strconv.ParseInt(bounds[0], 10, 64); err != nil || start < 0 {
		return 0, 0, 0, false
	}
	if end, err = strconv.ParseInt(bounds[1], 10, 64); err != nil || end < start {
		return 0, 0, 0, false
	}
	if total, err = strconv.ParseInt(parts[1], 10, 64); err != nil || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
