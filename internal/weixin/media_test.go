package weixin

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mediaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mediaPlanFixture(t *testing.T) *MediaUploadPlan {
	t.Helper()
	dir := mediaDir(t)
	source := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(source, []byte("the attachment's exact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareMediaUpload(context.Background(), MediaUploadRequest{Path: source, Name: "report.bin", ToUserID: "owner", Type: FileItemType}, MediaOptions{TempDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func uploadedFixture(plan *MediaUploadPlan) UploadedMedia {
	return UploadedMedia{
		FileKey: plan.FileKey, DownloadParam: "test-private-download", AESKeyHex: plan.AESKeyHex,
		MD5: plan.MD5, SHA256: plan.SHA256, Name: plan.Name, MIME: plan.MIME,
		ToUserID: plan.ToUserID, RawSize: plan.RawSize, CipherSize: plan.CipherSize, Type: plan.Type,
	}
}

func TestMediaFileAboveFormerLimitStreamsThroughStages(t *testing.T) {
	dir := mediaDir(t)
	path := filepath.Join(dir, "large.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// An actual 21 MiB source exercises the former 20 MiB boundary. Neither
	// plaintext nor ciphertext is assembled into an entire-file byte slice.
	const size = 21 * 1024 * 1024
	block := bytes.Repeat([]byte{0x42}, 32768)
	digest := sha256.New()
	for written := 0; written < size; written += len(block) {
		if _, err := io.MultiWriter(file, digest).Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	expectedSHA := fmt.Sprintf("%x", digest.Sum(nil))
	plan, err := PrepareMediaUpload(context.Background(), MediaUploadRequest{Path: path, Name: "large.bin", ToUserID: "owner", SHA256: expectedSHA}, MediaOptions{TempDir: dir})
	if err != nil || plan.RawSize != size || plan.CipherSize != size+16 {
		t.Fatalf("former limit blocked stream: plan=%v err=%v", plan != nil, err)
	}
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			if r.URL.Path != "/ilink/bot/getuploadurl" || r.Header.Get("Authorization") == "" {
				t.Fatal("incorrect upload-slot request")
			}
			var body struct {
				FileKey   string `json:"filekey"`
				RawSize   int64  `json:"rawsize"`
				FileSize  int64  `json:"filesize"`
				MediaType int    `json:"media_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.FileKey != plan.FileKey || body.RawSize != size || body.FileSize != size+16 || body.MediaType != 3 {
				t.Fatal("upload metadata does not describe prepared bytes")
			}
			return reply(r, 200, `{"ret":0,"upload_full_url":"https://novac2c.cdn.weixin.qq.com/c2c/upload?test=private"}`), nil
		case 2:
			if r.URL.Host != "novac2c.cdn.weixin.qq.com" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("AuthorizationType") != "" || r.ContentLength != plan.CipherSize || r.GetBody != nil {
				t.Fatal("CDN request retained authorization or lost stable bytes")
			}
			key, _ := hex.DecodeString(plan.AESKeyHex)
			uploadedSHA := sha256.New()
			plain, err := decryptECB(context.Background(), r.Body, uploadedSHA, key)
			if err != nil || plain != size || fmt.Sprintf("%x", uploadedSHA.Sum(nil)) != expectedSHA {
				t.Fatalf("CDN stream differs from original: plain=%d err=%v", plain, err)
			}
			response := reply(r, 200, "")
			response.Header.Set("x-encrypted-param", "test-private-download")
			return response, nil
		case 3:
			var body struct {
				Message Message `json:"msg"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Message.ClientID != "stable-media-id" || body.Message.ContextToken != " exact context " || len(body.Message.ItemList) != 1 || body.Message.ItemList[0].Type != FileItemType {
				t.Fatal("send changed media identity or context")
			}
			var metadata struct {
				Name   string `json:"file_name"`
				Length string `json:"len"`
			}
			if json.Unmarshal(body.Message.ItemList[0].FileItem, &metadata) != nil || metadata.Name != "large.bin" || metadata.Length != strconv.Itoa(size) {
				t.Fatal("file card lost original name or plaintext size")
			}
			return reply(r, 200, `{"ret":0,"message_id":18446744073709551615}`), nil
		default:
			t.Fatal("unexpected automatic retry")
			return nil, nil
		}
	})
	if err := c.RequestMediaUploadURL(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	uploaded, err := c.UploadPreparedMedia(context.Background(), plan, MediaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := c.SendMedia(context.Background(), SendMediaRequest{ToUserID: "owner", ClientID: "stable-media-id", ContextToken: " exact context ", Uploaded: *uploaded})
	if err != nil || ack.MessageID != "18446744073709551615" || requests != 3 {
		t.Fatalf("staged media ACK failed: err=%v requests=%d", err, requests)
	}
	if _, err := os.Stat(plan.CipherPath); err != nil {
		t.Fatal("upload removed bytes before journal owner could reconcile")
	}
}

func TestMediaDownloadFileImageVideoAndBothKeyEncodings(t *testing.T) {
	plan := mediaPlanFixture(t)
	for _, kind := range []int{FileItemType, ImageItemType, VideoItemType} {
		for _, rawKey := range []bool{false, true} {
			t.Run(fmt.Sprintf("kind-%d-raw-%v", kind, rawKey), func(t *testing.T) {
				uploaded := uploadedFixture(plan)
				uploaded.Type = kind
				item, err := BuildMediaItem(uploaded)
				if err != nil {
					t.Fatal(err)
				}
				var metadata map[string]any
				var field *json.RawMessage
				switch kind {
				case FileItemType:
					field = &item.FileItem
				case ImageItemType:
					field = &item.ImageItem
				case VideoItemType:
					field = &item.VideoItem
				}
				if err := json.Unmarshal(*field, &metadata); err != nil {
					t.Fatal(err)
				}
				if rawKey {
					key, _ := hex.DecodeString(plan.AESKeyHex)
					metadata["media"].(map[string]any)["aes_key"] = base64.StdEncoding.EncodeToString(key)
				}
				// File metadata declares plaintext len; all media use the actual
				// ciphertext length from HTTP. No thumbnail fallback is tested.
				metadata["md5"] = plan.MD5
				*field, _ = json.Marshal(metadata)
				requests := 0
				c := testClient(t, func(r *http.Request) (*http.Response, error) {
					requests++
					if r.Method != http.MethodGet || r.URL.Query().Get("encrypted_query_param") != uploaded.DownloadParam || r.Header.Get("Authorization") != "" {
						t.Fatal("unsafe media download request")
					}
					body, err := os.Open(plan.CipherPath)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header), ContentLength: plan.CipherSize, Request: r}, nil
				})
				file, err := c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: mediaDir(t)})
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(file.Path)
				if file.SHA256 != plan.SHA256 || file.MD5 != plan.MD5 || file.Size != plan.RawSize || file.Type != kind || requests != 1 {
					t.Fatal("download metadata/bytes differ")
				}
				info, _ := os.Stat(file.Path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("download file is not private")
				}
			})
		}
	}
}

func TestMediaURLAllocationUnknownAndRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		network bool
		want    error
	}{
		{"transport", 0, "", true, ErrOutcomeUnknown},
		{"server", 503, "test-secret", false, ErrOutcomeUnknown},
		{"bad-json", 200, `{"secret":`, false, ErrOutcomeUnknown},
		{"missing-url", 200, `{"ret":0}`, false, ErrOutcomeUnknown},
		{"expired", 200, `{"ret":-14}`, false, ErrSessionExpired},
		{"rejected", 200, `{"ret":-1,"errcode":99}`, false, ErrAPI},
		{"unsafe", 200, `{"ret":0,"upload_full_url":"https://evil.example/private-token"}`, false, ErrUnsafeURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := mediaPlanFixture(t)
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if tc.network {
					return nil, errors.New("private-token network error")
				}
				return reply(r, tc.status, tc.body), nil
			})
			err := c.RequestMediaUploadURL(context.Background(), plan)
			if !errors.Is(err, tc.want) || requests != 1 || plan.UploadURL != "" || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("allocation misclassified or leaked: %v requests=%d", err, requests)
			}
		})
	}
}

func TestMediaCDNUploadUnknownNoRetryAndPrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		network bool
		ack     bool
		want    error
	}{
		{"transport", 0, true, false, ErrOutcomeUnknown},
		{"server", 503, false, false, ErrOutcomeUnknown},
		{"timeout", 408, false, false, ErrOutcomeUnknown},
		{"no-download-param", 200, false, false, ErrOutcomeUnknown},
		{"platform-size-rejection", 413, false, false, ErrAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := mediaPlanFixture(t)
			plan.UploadURL = DefaultCDNURL + "/upload?encrypted_query_param=private-token"
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if tc.network {
					return nil, errors.New("private-token network error")
				}
				response := reply(r, tc.status, "private-token")
				response.Header.Set("x-error-message", "private-token")
				return response, nil
			})
			_, err := c.UploadPreparedMedia(context.Background(), plan, MediaOptions{})
			if !errors.Is(err, tc.want) || requests != 1 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("upload misclassified or retried: %v requests=%d", err, requests)
			}
			if _, err := os.Stat(plan.CipherPath); err != nil {
				t.Fatal("uncertain upload lost retained ciphertext")
			}
			if tc.status == 413 {
				var api *APIError
				if !errors.As(err, &api) || api.HTTPStatus != 413 {
					t.Fatal("platform status was discarded")
				}
			}
		})
	}
}

func TestMediaCDNRedirectDoesNotFollowOrAuthorize(t *testing.T) {
	plan := mediaPlanFixture(t)
	plan.UploadURL = DefaultCDNURL + "/upload?encrypted_query_param=private-token"
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "novac2c.cdn.weixin.qq.com" || r.Header.Get("Authorization") != "" {
			t.Fatal("CDN redirect followed or token forwarded")
		}
		response := reply(r, 307, "")
		response.Header.Set("Location", "https://evil.example/private-token")
		return response, nil
	})
	_, err := c.UploadPreparedMedia(context.Background(), plan, MediaOptions{})
	if !errors.Is(err, ErrOutcomeUnknown) || requests != 1 {
		t.Fatalf("redirect upload classification: %v requests=%d", err, requests)
	}
	item, _ := BuildMediaItem(uploadedFixture(plan))
	_, err = c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: mediaDir(t)})
	if !errors.Is(err, ErrUnsafeURL) || requests != 2 {
		t.Fatalf("redirect download classification: %v requests=%d", err, requests)
	}
}

func TestMediaPrepareCancellationHashFailureAndIntegrityCleanup(t *testing.T) {
	plan := mediaPlanFixture(t)
	for _, cancelled := range []bool{false, true} {
		dir := mediaDir(t)
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		_, err := PrepareMediaUpload(ctx, MediaUploadRequest{Path: plan.CipherPath, ToUserID: "owner", SHA256: strings.Repeat("0", 64)}, MediaOptions{TempDir: dir})
		cancel()
		want := ErrMediaIntegrity
		if cancelled {
			want = context.Canceled
		}
		if !errors.Is(err, want) {
			t.Fatalf("prepare failure was hidden: %v", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatal("failed prepare left partial ciphertext")
		}
	}
	if err := os.WriteFile(plan.CipherPath, bytes.Repeat([]byte{0}, int(plan.CipherSize)), 0600); err != nil {
		t.Fatal(err)
	}
	plan.UploadURL = DefaultCDNURL + "/upload?encrypted_query_param=private-token"
	c := testClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("altered snapshot was uploaded"); return nil, nil })
	if _, err := c.UploadPreparedMedia(context.Background(), plan, MediaOptions{}); !errors.Is(err, ErrMediaIntegrity) {
		t.Fatalf("altered snapshot accepted: %v", err)
	}
}

func TestMediaDownloadCorruptLengthAndCancelRemovePartialFiles(t *testing.T) {
	plan := mediaPlanFixture(t)
	item, _ := BuildMediaItem(uploadedFixture(plan))
	for _, tc := range []struct {
		name     string
		mismatch bool
		corrupt  bool
		cancel   bool
	}{
		{"length", true, false, false}, {"ciphertext", false, true, false}, {"cancel", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := mediaDir(t)
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				if tc.cancel {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				data, _ := os.ReadFile(plan.CipherPath)
				if tc.corrupt {
					data = data[:len(data)-1]
				}
				length := int64(len(data))
				if tc.mismatch {
					length++
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: length, Request: r}, nil
			})
			opts := MediaOptions{TempDir: dir}
			if tc.cancel {
				opts.TransferTimeout = time.Millisecond
			}
			_, err := c.DownloadMedia(context.Background(), item, opts)
			if tc.cancel {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("caller deadline hidden: %v", err)
				}
			} else if tc.mismatch {
				var readErr *MediaReadError
				if !errors.As(err, &readErr) || !readErr.Retryable || readErr.Reason != "resume-identity-unavailable" {
					t.Fatalf("incomplete response was misclassified: %v", err)
				}
			} else if !errors.Is(err, ErrMediaIntegrity) {
				t.Fatalf("corrupt media accepted: %v", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("failed download left partial plaintext")
			}
		})
	}
}

func TestMediaSendUnknownAndUnsupportedVoice(t *testing.T) {
	plan := mediaPlanFixture(t)
	for _, status := range []int{200, 503} {
		requests := 0
		c := testClient(t, func(r *http.Request) (*http.Response, error) { requests++; return reply(r, status, `{"ret":0}`), nil })
		_, err := c.SendMedia(context.Background(), SendMediaRequest{ToUserID: "owner", ClientID: "stable", ContextToken: "context", Uploaded: uploadedFixture(plan)})
		if !errors.Is(err, ErrOutcomeUnknown) || requests != 1 {
			t.Fatalf("media send unknown was hidden: %v requests=%d", err, requests)
		}
	}
	voice := uploadedFixture(plan)
	voice.Type = VoiceItemType
	if _, err := BuildMediaItem(voice); !errors.Is(err, ErrMediaUnsupported) {
		t.Fatalf("voice falsely advertised: %v", err)
	}
}

func TestMediaPlainImageAndUnsafeFullURL(t *testing.T) {
	data := []byte("plain image bytes supplied by Weixin")
	item := MessageItem{Type: ImageItemType, ImageItem: json.RawMessage(`{"media":{"full_url":"https://novac2c.cdn.weixin.qq.com/c2c/download?secret=test"}}`)}
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Header: make(http.Header), Request: r}, nil
	})
	file, err := c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: mediaDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if file.SHA256 != fmt.Sprintf("%x", want) {
		t.Fatal("plain image changed")
	}
	bad := MessageItem{Type: ImageItemType, ImageItem: json.RawMessage(`{"media":{"full_url":"https://weixin.qq.com.evil.example/secret"}}`)}
	if _, err := c.DownloadMedia(context.Background(), bad, MediaOptions{TempDir: mediaDir(t)}); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("unsafe full URL accepted: %v", err)
	}
	for _, raw := range []string{"http://novac2c.cdn.weixin.qq.com/file", "https://evil.example@novac2c.cdn.weixin.qq.com/file", "https://novac2c.cdn.weixin.qq.com:444/file", "https://127.0.0.1/file", "https://qq.com/file"} {
		if _, err := trustedMediaURL(raw); !errors.Is(err, ErrUnsafeURL) {
			t.Fatal("unsafe media origin accepted")
		}
	}
}

func TestMediaDownloadDeclaredMD5IsVerified(t *testing.T) {
	plan := mediaPlanFixture(t)
	item, _ := BuildMediaItem(uploadedFixture(plan))
	var metadata map[string]any
	_ = json.Unmarshal(item.FileItem, &metadata)
	wrong := md5.Sum([]byte("wrong original"))
	metadata["md5"] = fmt.Sprintf("%x", wrong)
	item.FileItem, _ = json.Marshal(metadata)
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		body, _ := os.Open(plan.CipherPath)
		return &http.Response{StatusCode: 200, Body: body, ContentLength: plan.CipherSize, Header: make(http.Header), Request: r}, nil
	})
	dir := mediaDir(t)
	if _, err := c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: dir}); !errors.Is(err, ErrMediaIntegrity) {
		t.Fatalf("MD5 mismatch accepted: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("hash failure left unverified media")
	}
}

func TestMediaDownloadVoicePreservesOriginalAndHeaderFormat(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantName, wantMIME, codec, container string
		encode                                          int
	}{
		{"silk-with-speex-declaration", "#!SILK_V3\x00exact voice", "voice.silk", "audio/silk", "silk", "silk", 4},
		{"tencent-silk", "\x02#!SILK_V3\x00exact voice", "voice.silk", "audio/silk", "silk", "silk", 6},
		{"raw-speex", "\x11\x22\x33\x44exact voice", "voice.speex", "audio/speex", "speex", "raw", 4},
		{"ogg-speex", "OggS\x00exact voice", "voice.ogg", "audio/ogg", "ogg-speex", "ogg", 8},
		{"wav", "RIFF1234WAVEexact voice", "voice.wav", "audio/wav", "pcm", "wav", 1},
		{"amr", "#!AMR\nexact voice", "voice.amr", "audio/amr", "amr", "amr", 5},
		{"amr-wb", "#!AMR-WB\nexact voice", "voice.awb", "audio/amr-wb", "amr", "amr-wb", 5},
		{"mp3", "ID3exact voice", "voice.mp3", "audio/mpeg", "mp3", "mp3", 7},
		{"unknown", "\x11\x22\x33\x44exact voice", "voice.raw", "application/octet-stream", "unknown", "raw", 91},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := []byte("0123456789abcdef")
			var encrypted bytes.Buffer
			if _, _, err := encryptECB(context.Background(), strings.NewReader(tc.raw), &encrypted, key); err != nil {
				t.Fatal(err)
			}
			wire := map[string]any{
				"media":       map[string]any{"encrypt_query_param": "test-private", "aes_key": base64.StdEncoding.EncodeToString(key)},
				"encode_type": tc.encode, "bits_per_sample": 16, "sample_rate": 16000,
				// Metadata has no invented recording-duration ceiling.
				"playtime": int64(9223372036854775807), "text": "source supplied transcript",
			}
			raw, _ := json.Marshal(wire)
			item := MessageItem{Type: VoiceItemType, VoiceItem: raw}
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
					t.Fatal("voice CDN request retained credentials")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encrypted.Bytes())), ContentLength: int64(encrypted.Len()), Header: make(http.Header), Request: r}, nil
			})
			file, err := c.DownloadMedia(context.Background(), item, MediaOptions{TempDir: mediaDir(t)})
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(file.Path)
			original, err := os.ReadFile(file.Path)
			wantSHA := sha256.Sum256([]byte(tc.raw))
			if err != nil || string(original) != tc.raw || file.SHA256 != fmt.Sprintf("%x", wantSHA) || file.Size != int64(len(tc.raw)) {
				t.Fatal("voice original was replaced, transformed or misidentified")
			}
			if requests != 1 || file.Type != VoiceItemType || file.Name != tc.wantName || file.MIME != tc.wantMIME || file.Voice == nil {
				t.Fatal("voice download lost its typed source format")
			}
			if file.Voice.EncodeType != tc.encode || file.Voice.BitsPerSample != 16 || file.Voice.SampleRate != 16000 || file.Voice.PlaytimeMS != 9223372036854775807 || file.Voice.Codec != tc.codec || file.Voice.Container != tc.container {
				t.Fatal("voice metadata or header-derived format changed")
			}
			if tc.encode == 4 && file.Voice.DeclaredCodec != "speex" {
				t.Fatal("header correction erased source declaration")
			}
		})
	}
}

func TestMediaVoiceMalformedMetadataFailsBeforeDownload(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	for _, wire := range []string{
		`{"media":{"encrypt_query_param":"private"},"encode_type":4}`,
		`{"media":{"encrypt_query_param":"private","aes_key":"` + key + `"},"encode_type":-1}`,
		`{"media":{"encrypt_query_param":"private","aes_key":"` + key + `"},"playtime":-1}`,
		`{"media":{"encrypt_query_param":"private","aes_key":"` + key + `"},"sample_rate":-1}`,
		`{"media":{"encrypt_query_param":"private","aes_key":"` + key + `"},"bits_per_sample":-1}`,
	} {
		c := testClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid voice reached CDN"); return nil, nil })
		_, err := c.DownloadMedia(context.Background(), MessageItem{Type: VoiceItemType, VoiceItem: json.RawMessage(wire)}, MediaOptions{TempDir: mediaDir(t)})
		if !errors.Is(err, ErrMediaIntegrity) {
			t.Fatalf("malformed voice accepted: %v", err)
		}
	}
}
