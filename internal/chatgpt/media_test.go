package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mediaTestToken(marker string) string {
	claims := map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"marker":                      marker,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account", "chatgpt_user_id": "user"},
	}
	data, _ := json.Marshal(claims)
	return "header." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
}
func mediaTestClient(t *testing.T, transport mediaRoundTrip) *Client {
	t.Helper()
	c, err := New(Credentials{AccessToken: mediaTestToken("original"), SessionToken: "session"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	c.rooms["room"] = true
	return c
}
func mediaResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req, ContentLength: int64(len(body))}
}
func uploadedPart(t *testing.T, req *http.Request) *multipart.Part {
	t.Helper()
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	part, err := multipart.NewReader(req.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	return part
}

type mediaCountWriter struct {
	bytes   int64
	max     int
	cancel  context.CancelFunc
	failure error
}

func (w *mediaCountWriter) Write(data []byte) (int, error) {
	if w.failure != nil {
		return 0, w.failure
	}
	w.bytes += int64(len(data))
	if len(data) > w.max {
		w.max = len(data)
	}
	if w.cancel != nil {
		w.cancel()
	}
	return len(data), nil
}

type mediaZeros struct{}

func (mediaZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type mediaTrackedBody struct {
	io.Reader
	closed bool
}

func (b *mediaTrackedBody) Close() error { b.closed = true; return nil }

func TestUploadFileAboveOldLimitStreams(t *testing.T) {
	const size = int64(21 << 20)
	localPath := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/backend-api/messaging/rooms/room/files" || req.Header.Get("ChatGPT-Account-Id") != "account" {
			t.Fatalf("unexpected upload request: %s", req.URL.Path)
		}
		if _, ok := req.Context().Deadline(); ok {
			t.Fatal("media inherited a fixed total timeout")
		}
		part := uploadedPart(t, req)
		if part.FileName() != "_large.bin" || part.Header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("unexpected multipart metadata: %q", part.FileName())
		}
		count := &mediaCountWriter{}
		n, err := io.Copy(count, part)
		if err != nil || n != size || count.max > 32768 {
			t.Fatalf("unbounded or incomplete transfer: n=%d max=%d err=%v", n, count.max, err)
		}
		if req.ContentLength <= size {
			t.Fatal("multipart content length missing")
		}
		return mediaResponse(req, 200, `{"id":"file"}`), nil
	})
	c.http.Timeout = time.Millisecond
	id, err := c.UploadFile(context.Background(), "room", "/large.bin", "", localPath)
	if err != nil || id != "file" || calls != 1 {
		t.Fatalf("id=%q calls=%d err=%v", id, calls, err)
	}
	assertSourceClosed(t, localPath)
}

func TestUploadUTF8QuotedFilenamePreservesOriginalAndRejectsHeaderInjection(t *testing.T) {
	for _, tc := range []struct{ name, cleaned string }{
		{`中文原件"最终".zip`, `中文原件"最终".zip`},
		{"中文原件\r\nX-Injected: yes\".zip", "中文原件__X-Injected: yes\".zip"},
		{"目录\\原件/文件\x00\x7f.zip", "目录_原件_文件__.zip"},
	} {
		t.Run(tc.cleaned, func(t *testing.T) {
			original := []byte{0, 255, 'P', 'K', 3, 4, '\r', '\n', 128}
			localPath := filepath.Join(t.TempDir(), "original.bin")
			if err := os.WriteFile(localPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			// The standard library is the compatibility oracle for the header,
			// while this uploader separately preserves the declared MIME type.
			var standard bytes.Buffer
			oracle := multipart.NewWriter(&standard)
			if _, err := oracle.CreateFormFile("file", tc.cleaned); err != nil {
				t.Fatal(err)
			}
			if err := oracle.Close(); err != nil {
				t.Fatal(err)
			}
			standardPart, err := multipart.NewReader(&standard, oracle.Boundary()).NextPart()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				wire, err := io.ReadAll(req.Body)
				if err != nil || int64(len(wire)) != req.ContentLength {
					t.Fatal("multipart body length changed")
				}
				_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				reader := multipart.NewReader(bytes.NewReader(wire), params["boundary"])
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				disposition := part.Header.Get("Content-Disposition")
				if strings.Contains(disposition, "filename*") || disposition != standardPart.Header.Get("Content-Disposition") || part.FileName() != tc.cleaned || part.FormName() != "file" ||
					part.Header.Get("Content-Type") != "application/zip" || len(part.Header) != 2 {
					t.Fatal("UTF-8 name, escaping, MIME or header isolation changed")
				}
				got, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("multipart encoding changed the original file bytes")
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Fatal("filename injected another multipart part")
				}
				return mediaResponse(req, 200, `{"id":"verified-file"}`), nil
			})
			id, err := c.UploadFile(context.Background(), "room", tc.name, "application/zip", localPath)
			if err != nil || id != "verified-file" || calls != 1 {
				t.Fatalf("UTF-8 upload failed: calls=%d err=%v", calls, err)
			}
			assertSourceClosed(t, localPath)
		})
	}
}

func TestUploadUnknownNeverReplayed(t *testing.T) {
	for _, scenario := range []string{"network", "server", "timeout-response", "redirect", "missing-id", "malformed", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				_, _ = io.Copy(io.Discard, req.Body)
				switch scenario {
				case "network":
					return nil, errors.New("connection dropped")
				case "server":
					return mediaResponse(req, 503, "busy"), nil
				case "timeout-response":
					return mediaResponse(req, 408, "timeout"), nil
				case "redirect":
					return mediaResponse(req, 307, "redirect"), nil
				case "missing-id":
					return mediaResponse(req, 200, `{}`), nil
				case "malformed":
					return mediaResponse(req, 200, `{`), nil
				case "cancel":
					cancel()
					return nil, context.Canceled
				}
				panic("unexpected scenario")
			})
			_, err := c.Upload(ctx, "room", "file.txt", "text/plain", []byte("original"))
			var unknown *UploadOutcomeUnknownError
			if !errors.As(err, &unknown) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestUploadKnown401ReplaysSameOpenedFile(t *testing.T) {
	directory := t.TempDir()
	localPath, movedPath := filepath.Join(directory, "source.txt"), filepath.Join(directory, "moved.txt")
	if err := os.WriteFile(localPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	attempts, renewals := 0, 0
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/auth/session" {
			renewals++
			body, _ := json.Marshal(map[string]string{"accessToken": mediaTestToken("renewed"), "sessionToken": "renewed-session"})
			return mediaResponse(req, 200, string(body)), nil
		}
		attempts++
		part := uploadedPart(t, req)
		body, err := io.ReadAll(part)
		if err != nil || string(body) != "original" {
			t.Fatalf("replay changed file bytes: %q %v", body, err)
		}
		if attempts == 1 {
			if err := os.Rename(localPath, movedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localPath, []byte("replaced"), 0600); err != nil {
				t.Fatal(err)
			}
			return mediaResponse(req, 401, "unauthorized"), nil
		}
		return mediaResponse(req, 200, `{"id":"verified-file"}`), nil
	})
	id, err := c.UploadFile(context.Background(), "room", "file.txt", "text/plain", localPath)
	if err != nil || id != "verified-file" || attempts != 2 || renewals != 1 {
		t.Fatalf("id=%s attempts=%d renewals=%d err=%v", id, attempts, renewals, err)
	}
	assertSourceClosed(t, movedPath)
}

func TestUploadFileUnknownClosesSource(t *testing.T) {
	localPath := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(localPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, req.Body)
		cancel()
		return nil, context.Canceled
	})
	_, err := c.UploadFile(ctx, "room", "source.txt", "text/plain", localPath)
	var unknown *UploadOutcomeUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must retain unknown outcome: %v", err)
	}
	assertSourceClosed(t, localPath)
}

func TestUploadRejectsUnverifiedRoomAndPreCancelledContext(t *testing.T) {
	calls := 0
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return mediaResponse(req, 200, `{"id":"file"}`), nil
	})
	if _, err := c.UploadFile(context.Background(), "other-room", "f", "", "missing"); err == nil {
		t.Fatal("unverified room accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Upload(ctx, "room", "f", "", []byte("bytes"))
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestLongUploadDoesNotLockMessageReads(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			close(started)
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return mediaResponse(req, 200, `{"id":"file"}`), nil
		}
		return mediaResponse(req, 200, `{"items":[]}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	uploaded := make(chan error, 1)
	go func() { _, err := c.Upload(ctx, "room", "f", "", []byte("bytes")); uploaded <- err }()
	<-started
	read := make(chan error, 1)
	go func() { _, err := c.Messages(ctx, "room", ""); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("media upload blocked ordinary message reads")
	}
	close(release)
	if err := <-uploaded; err != nil {
		t.Fatal(err)
	}
}

func assertSourceClosed(t *testing.T, localPath string) {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, entry := range entries {
		target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if target == localPath {
			t.Fatalf("source descriptor remained open: %s", entry.Name())
		}
	}
}

func mediaMetadata(req *http.Request, fileID, downloadURL string) *http.Response {
	data, _ := json.Marshal(map[string]string{"id": fileID, "download_url": downloadURL, "name": "/large.bin", "mime_type": "application/octet-stream"})
	return mediaResponse(req, 200, string(data))
}

func TestDownloadAboveOldLimitStreamsAndKeepsStoragePrivate(t *testing.T) {
	const size = int64(21 << 20)
	body := &mediaTrackedBody{Reader: io.LimitReader(mediaZeros{}, size)}
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "chatgpt.com" {
			return mediaMetadata(req, "file", "https://storage.oaiusercontent.com/object?private-signature=value"), nil
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("ChatGPT-Account-Id") != "" {
			t.Fatal("credentials sent to attachment storage")
		}
		if _, ok := req.Context().Deadline(); ok {
			t.Fatal("media inherited a fixed total timeout")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: size}, nil
	})
	writer := &mediaCountWriter{}
	info, err := c.DownloadAttachmentTo(context.Background(), "room", "file", writer)
	if err != nil || info.Size != size || writer.bytes != size || writer.max > 32768 || !body.closed {
		t.Fatalf("info=%+v size=%d max=%d closed=%v err=%v", info, writer.bytes, writer.max, body.closed, err)
	}
	if info.Name != "_large.bin" || info.MIME != "application/octet-stream" {
		t.Fatalf("metadata: %+v", info)
	}
}

func TestDownloadRejectsStorageOriginsAndIdentityMismatch(t *testing.T) {
	for _, downloadURL := range []string{"http://storage.oaiusercontent.com/a", "https://oaiusercontent.com/a", "https://storage.oaiusercontent.com.evil/a", "https://user@storage.oaiusercontent.com/a", "https://storage.oaiusercontent.com:443/a", "https://storage.oaiusercontent.com/a#fragment", "https://127.0.0.1/a"} {
		t.Run(downloadURL, func(t *testing.T) {
			calls := 0
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				return mediaMetadata(req, "file", downloadURL), nil
			})
			_, err := c.DownloadAttachmentTo(context.Background(), "room", "file", io.Discard)
			var unavailable *AttachmentUnavailableError
			if !errors.As(err, &unavailable) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
	calls := 0
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return mediaMetadata(req, "different-file", "https://storage.oaiusercontent.com/a"), nil
	})
	if _, err := c.DownloadAttachmentTo(context.Background(), "room", "file", io.Discard); err == nil || calls != 1 {
		t.Fatalf("metadata mismatch not refused: calls=%d err=%v", calls, err)
	}
}

func TestDownloadDoesNotFollowStorageRedirect(t *testing.T) {
	storageCalls := 0
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "chatgpt.com" {
			return mediaMetadata(req, "file", "https://storage.oaiusercontent.com/a"), nil
		}
		storageCalls++
		response := mediaResponse(req, 302, "redirect")
		response.Header.Set("Location", "https://evil.example/a")
		return response, nil
	})
	if _, err := c.DownloadAttachmentTo(context.Background(), "room", "file", io.Discard); err == nil || storageCalls != 1 {
		t.Fatalf("storageCalls=%d err=%v", storageCalls, err)
	}
}

func TestDownloadTemporaryReadFailuresRemainRetryable(t *testing.T) {
	for _, scenario := range []string{"metadata-network", "not-ready", "network", "busy", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host == "chatgpt.com" {
					if scenario == "metadata-network" {
						return nil, errors.New("private metadata transport detail")
					}
					if scenario == "not-ready" {
						return mediaMetadata(req, "file", ""), nil
					}
					return mediaMetadata(req, "file", "https://storage.oaiusercontent.com/a?capability=private"), nil
				}
				if scenario == "network" {
					return nil, errors.New("private transport detail")
				}
				status := 503
				if scenario == "denied" {
					status = 403
				}
				return mediaResponse(req, status, "provider detail"), nil
			})
			_, err := c.DownloadAttachmentTo(context.Background(), "room", "file", io.Discard)
			var read *AttachmentReadError
			if !errors.As(err, &read) || read.Retryable != (scenario != "denied") {
				t.Fatalf("read classification: %v", err)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "capability") {
				t.Fatal("storage capability or raw transport detail escaped")
			}
			wantCalls := 2
			if scenario == "not-ready" || scenario == "metadata-network" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("unexpected implicit GET retry: %d", calls)
			}
		})
	}
}

func TestDownloadCancellationWriterFailureAndLengthMismatch(t *testing.T) {
	for _, scenario := range []string{"cancel", "writer", "short"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &mediaTrackedBody{Reader: io.LimitReader(mediaZeros{}, 65536)}
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "chatgpt.com" {
					return mediaMetadata(req, "file", "https://storage.oaiusercontent.com/a"), nil
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: 65537}, nil
			})
			writer := &mediaCountWriter{}
			if scenario == "cancel" {
				writer.cancel = cancel
			}
			if scenario == "writer" {
				writer.failure = io.ErrShortWrite
			}
			info, err := c.DownloadAttachmentTo(ctx, "room", "file", writer)
			if err == nil || !body.closed || info == nil || info.Size != writer.bytes {
				t.Fatalf("info=%+v closed=%v err=%v", info, body.closed, err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if scenario == "writer" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("writer failure lost: %v", err)
			}
		})
	}
}

func TestLegacyDownloadPreservesBytes(t *testing.T) {
	c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "chatgpt.com" {
			return mediaMetadata(req, "file", "https://storage.oaiusercontent.com/a"), nil
		}
		return mediaResponse(req, 200, "original bytes"), nil
	})
	file, err := c.DownloadAttachment(context.Background(), "room", "file")
	if err != nil || string(file.Data) != "original bytes" || file.MIME != "text/plain" {
		t.Fatalf("file=%+v err=%v", file, err)
	}
}

func ExampleUploadOutcomeUnknownError() {
	var unknown *UploadOutcomeUnknownError
	err := &UploadOutcomeUnknownError{Cause: context.Canceled}
	fmt.Println(errors.As(err, &unknown), errors.Is(err, context.Canceled))
	// Output: true true
}
