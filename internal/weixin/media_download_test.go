package weixin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type interruptedMediaReader struct{ io.Reader }

func (r interruptedMediaReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("transport failed at https://secret.example/private-token")
	}
	return n, err
}

func mediaItemWithOriginalMD5(t *testing.T, plan *MediaUploadPlan) MessageItem {
	t.Helper()
	item, err := BuildMediaItem(uploadedFixture(plan))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(item.FileItem, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["md5"] = plan.MD5
	item.FileItem, _ = json.Marshal(metadata)
	return item
}

func TestMediaDownloadResumesExactCiphertextAfterInterruptedUnalignedRead(t *testing.T) {
	dir := mediaDir(t)
	source := filepath.Join(dir, "source.bin")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// The complete original exceeds a transport window, and interruption is
	// deliberately between AES blocks. Resume occurs before any decryption.
	const rawSize = mediaDownloadRangeBytes + 213
	if _, err := io.CopyBuffer(file, io.LimitReader(repeatedMediaByte(0x57), rawSize), make([]byte, 32768)); err != nil {
		t.Fatal(err)
	}
	file.Close()
	plan, err := PrepareMediaUpload(context.Background(), MediaUploadRequest{Path: source, Name: "source.bin", ToUserID: "owner"}, MediaOptions{TempDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var downloaded int64
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("If-Range") != "" {
			t.Fatal("resume request changed authentication or representation")
		}
		body, err := os.Open(plan.CipherPath)
		if err != nil {
			t.Fatal(err)
		}
		response := &http.Response{StatusCode: 200, Body: body, ContentLength: plan.CipherSize, Header: make(http.Header), Request: r}
		if requests == 1 {
			if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", mediaDownloadRangeBytes-1) {
				t.Fatal("known original digest did not bound the initial request")
			}
			downloaded = 37
			response.Body = struct {
				io.Reader
				io.Closer
			}{interruptedMediaReader{io.LimitReader(body, downloaded)}, body}
			return response, nil
		}
		end := plan.CipherSize - 1
		if plan.CipherSize-downloaded > mediaDownloadRangeBytes {
			end = downloaded + mediaDownloadRangeBytes - 1
		}
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", downloaded, end) {
			t.Fatal("resume skipped or repeated ciphertext")
		}
		body.Seek(downloaded, io.SeekStart)
		response.StatusCode = http.StatusPartialContent
		response.ContentLength = end - downloaded + 1
		response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", downloaded, end, plan.CipherSize))
		length := response.ContentLength
		if requests == 2 {
			// A second interruption must retain its additional byte progress.
			length = 65553
			response.Body = struct {
				io.Reader
				io.Closer
			}{interruptedMediaReader{io.LimitReader(body, length)}, body}
		} else {
			response.Body = struct {
				io.Reader
				io.Closer
			}{io.LimitReader(body, length), body}
		}
		downloaded += length
		return response, nil
	})
	downloadDir := mediaDir(t)
	got, err := c.DownloadMedia(context.Background(), mediaItemWithOriginalMD5(t, plan), MediaOptions{TempDir: downloadDir})
	if err != nil || requests < 3 || got == nil || got.Size != rawSize || got.SHA256 != plan.SHA256 || got.MD5 != plan.MD5 {
		t.Fatalf("resumed bytes differ: requests=%d err=%v", requests, err)
	}
	entries, _ := os.ReadDir(downloadDir)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(got.Path) {
		t.Fatal("completed ciphertext temporary file was not cleaned")
	}
}

func TestMediaDownloadInitialRangeCanEndAtActualEOF(t *testing.T) {
	plan := mediaPlanFixture(t)
	data, _ := os.ReadFile(plan.CipherPath)
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if requests != 1 || r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", mediaDownloadRangeBytes-1) {
			t.Fatal("initial bounded download was not requested")
		}
		header := make(http.Header)
		header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
		return &http.Response{StatusCode: 206, Header: header, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	})
	got, err := c.DownloadMedia(context.Background(), mediaItemWithOriginalMD5(t, plan), MediaOptions{TempDir: mediaDir(t)})
	if err != nil || got == nil || got.SHA256 != plan.SHA256 || requests != 1 {
		t.Fatalf("range ending before transport window was rejected: %v", err)
	}
}

func TestMediaDownloadInvalidRangeOrNoProgressStopsAndCleans(t *testing.T) {
	plan := mediaPlanFixture(t)
	data, _ := os.ReadFile(plan.CipherPath)
	for _, tc := range []struct {
		name, reason string
		integrity    bool
	}{
		{"wrong-start", "", true}, {"wrong-end", "", true}, {"wrong-total", "", true},
		{"wrong-length", "", true}, {"overlong", "", true},
		{"range-unsupported", "range-unsupported", false}, {"no-progress", "no-progress", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if requests > 2 {
					t.Fatal("invalid or empty response entered a blind retry loop")
				}
				response := &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: make(http.Header), Request: r, Body: io.NopCloser(interruptedMediaReader{bytes.NewReader(data[:7])})}
				if requests == 1 {
					return response, nil
				}
				response.StatusCode = 206
				start, end, total := int64(7), int64(len(data)-1), int64(len(data))
				switch tc.name {
				case "wrong-start":
					start++
				case "wrong-end":
					end--
				case "wrong-total":
					total++
				case "range-unsupported":
					response.StatusCode = 200
				}
				response.ContentLength = end - start + 1
				response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
				if tc.name == "wrong-length" {
					response.ContentLength++
				}
				payload := data[7:]
				if tc.name == "no-progress" {
					payload = nil
				} else if tc.name == "overlong" {
					payload = append(bytes.Clone(payload), 1)
				}
				response.Body = io.NopCloser(bytes.NewReader(payload))
				return response, nil
			})
			dir := mediaDir(t)
			_, err := c.DownloadMedia(context.Background(), mediaItemWithOriginalMD5(t, plan), MediaOptions{TempDir: dir})
			if tc.integrity {
				if !errors.Is(err, ErrMediaIntegrity) {
					t.Fatalf("invalid range accepted: %v", err)
				}
			} else {
				var readErr *MediaReadError
				if !errors.As(err, &readErr) || readErr.Reason != tc.reason || readErr.Retryable != (tc.name == "no-progress") {
					t.Fatalf("read failure classification: %v", err)
				}
			}
			if strings.Contains(err.Error(), "private-token") || requests != 2 {
				t.Fatal("unsafe diagnostic or incorrect request count")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("failed resume retained unverified files")
			}
		})
	}
}

func TestMediaDownloadStrongETagResumeAndChangedEntity(t *testing.T) {
	plan := mediaPlanFixture(t)
	data, _ := os.ReadFile(plan.CipherPath)
	item, _ := BuildMediaItem(uploadedFixture(plan)) // no source MD5
	for _, changed := range []bool{false, true} {
		requests := 0
		c := testClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			response := &http.Response{StatusCode: 200, Header: make(http.Header), Request: r}
			response.Header.Set("ETag", `"original"`)
			if requests == 1 {
				response.ContentLength = int64(len(data))
				response.Body = io.NopCloser(interruptedMediaReader{bytes.NewReader(data[:9])})
				return response, nil
			}
			if requests != 2 || r.Header.Get("If-Range") != `"original"` {
				t.Fatal("strong entity validator was not retained")
			}
			response.StatusCode = 206
			response.ContentLength = int64(len(data) - 9)
			response.Header.Set("Content-Range", fmt.Sprintf("bytes 9-%d/%d", len(data)-1, len(data)))
			response.Body = io.NopCloser(bytes.NewReader(data[9:]))
			if changed {
				response.Header.Set("ETag", `"changed"`)
			}
			return response, nil
		})
		got, err := c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: mediaDir(t)})
		if changed && !errors.Is(err, ErrMediaIntegrity) || !changed && (err != nil || got.SHA256 != plan.SHA256) {
			t.Fatalf("entity validation failure: changed=%t err=%v", changed, err)
		}
	}
}

func TestMediaCryptoReadErrorsDoNotExposeSource(t *testing.T) {
	_, err := decryptECB(context.Background(), interruptedMediaReader{bytes.NewReader(make([]byte, 19))}, io.Discard, []byte("0123456789abcdef"))
	var readErr *mediaCryptoReadError
	if !errors.As(err, &readErr) || errors.Is(err, ErrMediaIntegrity) || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("reader failure was hidden or disclosed: %v", err)
	}
}
