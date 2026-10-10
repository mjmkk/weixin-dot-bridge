// Media protocol adapted from Tencent/openclaw-weixin, frozen SourceCommit
// (MIT). See docs/weixin-source.md for attribution and the permission notice.
package weixin

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ImageItemType = 2
	VoiceItemType = 3
	FileItemType  = 4
	VideoItemType = 5
)

var (
	ErrMediaUnsupported = errors.New("Weixin media type is not implemented")
	ErrMediaIntegrity   = errors.New("Weixin media bytes or metadata failed verification")
)

// TempDir must be a private directory. Zero TransferTimeout means that only
// the caller's context controls the transfer; the ordinary API timeout is not
// reused for potentially large files.
type MediaOptions struct {
	TempDir         string
	TransferTimeout time.Duration
}

type MediaFile struct {
	Path   string       `json:"path"`
	Name   string       `json:"name"`
	MIME   string       `json:"mime"`
	MD5    string       `json:"md5"`
	SHA256 string       `json:"sha256"`
	Size   int64        `json:"size"`
	Type   int          `json:"type"`
	Voice  *VoiceFormat `json:"voice,omitempty"`
}

type MediaUploadRequest struct {
	Path, Name, MIME, ToUserID string
	Type                       int
	// Optional expected plaintext digest, checked before any network operation.
	SHA256 string
}

// A plan is secret: AESKeyHex and UploadURL must remain in the private journal.
// Preparing, requesting a slot, uploading bytes and sending a message are
// separate operations so the caller can persist each impending side effect.
type MediaUploadPlan struct {
	FileKey      string `json:"file_key"`
	AESKeyHex    string `json:"aes_key_hex"`
	CipherPath   string `json:"cipher_path"`
	MD5          string `json:"md5"`
	SHA256       string `json:"sha256"`
	CipherSHA256 string `json:"cipher_sha256"`
	Name         string `json:"name"`
	MIME         string `json:"mime"`
	ToUserID     string `json:"to_user_id"`
	UploadURL    string `json:"upload_url,omitempty"`
	RawSize      int64  `json:"raw_size"`
	CipherSize   int64  `json:"cipher_size"`
	Type         int    `json:"type"`
}

// UploadedMedia is also secret. Its AES key and download parameter grant access
// to the attachment, and must never enter ordinary progress or error logs.
type UploadedMedia struct {
	FileKey       string `json:"file_key"`
	DownloadParam string `json:"download_param"`
	AESKeyHex     string `json:"aes_key_hex"`
	MD5           string `json:"md5"`
	SHA256        string `json:"sha256"`
	Name          string `json:"name"`
	MIME          string `json:"mime"`
	ToUserID      string `json:"to_user_id"`
	RawSize       int64  `json:"raw_size"`
	CipherSize    int64  `json:"cipher_size"`
	Type          int    `json:"type"`
}

type SendMediaRequest struct {
	ToUserID, ContextToken, ClientID, RunID string
	Uploaded                                UploadedMedia
}

type cdnReference struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"`
}

type inboundMedia struct {
	Media         *cdnReference `json:"media"`
	AESKey        string        `json:"aeskey"`
	Name          string        `json:"file_name"`
	MD5           string        `json:"md5"`
	Length        string        `json:"len"`
	Type          int           `json:"-"`
	RawSize       *int64        `json:"-"`
	EncodeType    int           `json:"encode_type"`
	BitsPerSample int           `json:"bits_per_sample"`
	SampleRate    int           `json:"sample_rate"`
	PlaytimeMS    int64         `json:"playtime"`
}

func mediaUploadType(kind int) (int, error) {
	switch kind {
	case ImageItemType:
		return 1, nil
	case VideoItemType:
		return 2, nil
	case FileItemType:
		return 3, nil
	default:
		return 0, ErrMediaUnsupported
	}
}

func privateMediaTemp(dir, pattern string) (*os.File, error) {
	if dir == "" {
		return nil, ErrInvalidRequest
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot create private Weixin media directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Weixin media directory must be private (0700)")
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, errors.New("cannot create private Weixin media file")
	}
	return file, nil
}

func cleanMediaName(name, fallback string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	return name
}

// PrepareMediaUpload makes one immutable encrypted snapshot, hashing the exact
// original bytes as they are consumed. Memory stays bounded regardless of size.
// Success retains CipherPath for the caller; failures remove partial output.
func PrepareMediaUpload(ctx context.Context, input MediaUploadRequest, opts MediaOptions) (*MediaUploadPlan, error) {
	if input.Path == "" || input.ToUserID == "" || opts.TransferTimeout < 0 {
		return nil, ErrInvalidRequest
	}
	ctx, cancel, err := mediaContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if input.Name == "" {
		input.Name = filepath.Base(input.Path)
	}
	input.Name = cleanMediaName(input.Name, "file.bin")
	if input.MIME == "" {
		input.MIME = mime.TypeByExtension(strings.ToLower(filepath.Ext(input.Name)))
		if input.MIME == "" {
			input.MIME = "application/octet-stream"
		}
	}
	if input.Type == 0 {
		switch {
		case strings.HasPrefix(input.MIME, "image/"):
			input.Type = ImageItemType
		case strings.HasPrefix(input.MIME, "video/"):
			input.Type = VideoItemType
		default:
			input.Type = FileItemType
		}
	}
	if _, err := mediaUploadType(input.Type); err != nil {
		return nil, err
	}
	source, err := os.Open(input.Path)
	if err != nil {
		return nil, errors.New("cannot open Weixin media source")
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrInvalidRequest
	}
	var key, fileKey [16]byte
	if _, err = rand.Read(key[:]); err != nil {
		return nil, errors.New("cannot prepare Weixin media encryption")
	}
	if _, err = rand.Read(fileKey[:]); err != nil {
		return nil, errors.New("cannot prepare Weixin media identity")
	}
	output, err := privateMediaTemp(opts.TempDir, "weixin-cipher-*")
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		output.Close()
		if !completed {
			os.Remove(output.Name())
		}
	}()
	plainMD5, plainSHA, cipherSHA := md5.New(), sha256.New(), sha256.New()
	rawSize, cipherSize, err := encryptECB(ctx, io.TeeReader(source, io.MultiWriter(plainMD5, plainSHA)), io.MultiWriter(output, cipherSHA), key[:])
	if err != nil {
		return nil, err
	}
	digest := hex.EncodeToString(plainSHA.Sum(nil))
	if rawSize != info.Size() || (input.SHA256 != "" && !strings.EqualFold(input.SHA256, digest)) {
		return nil, ErrMediaIntegrity
	}
	if err = output.Sync(); err != nil {
		return nil, errors.New("cannot persist Weixin media snapshot")
	}
	if err = output.Close(); err != nil {
		return nil, errors.New("cannot close Weixin media snapshot")
	}
	completed = true
	return &MediaUploadPlan{
		FileKey: hex.EncodeToString(fileKey[:]), AESKeyHex: hex.EncodeToString(key[:]),
		CipherPath: output.Name(), MD5: hex.EncodeToString(plainMD5.Sum(nil)), SHA256: digest,
		CipherSHA256: hex.EncodeToString(cipherSHA.Sum(nil)), Name: input.Name, MIME: input.MIME,
		ToUserID: input.ToUserID, RawSize: rawSize, CipherSize: cipherSize, Type: input.Type,
	}, nil
}

func validMediaPlan(plan *MediaUploadPlan) error {
	if plan == nil || plan.FileKey == "" || plan.CipherPath == "" || plan.ToUserID == "" || plan.RawSize < 0 || plan.CipherSize <= 0 {
		return ErrInvalidRequest
	}
	if _, err := mediaUploadType(plan.Type); err != nil {
		return err
	}
	if key, err := hex.DecodeString(plan.AESKeyHex); err != nil || len(key) != 16 {
		return ErrMediaIntegrity
	}
	if digest, err := hex.DecodeString(plan.MD5); err != nil || len(digest) != 16 {
		return ErrMediaIntegrity
	}
	if digest, err := hex.DecodeString(plan.CipherSHA256); err != nil || len(digest) != 32 {
		return ErrMediaIntegrity
	}
	// Compare quotient form to avoid overflow near the int64 filesystem limit.
	if plan.CipherSize%16 != 0 || plan.CipherSize/16 != plan.RawSize/16+1 {
		return ErrMediaIntegrity
	}
	return nil
}

// Only Tencent-hosted HTTPS media targets are accepted; CDN requests carry no
// bot Authorization header. Even redirects to another trusted host are refused.
func trustedMediaURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, ErrUnsafeURL
	}
	host := strings.ToLower(u.Hostname())
	if host != "weixin.qq.com" && !strings.HasSuffix(host, ".weixin.qq.com") {
		return nil, ErrUnsafeURL
	}
	if host == "" || strings.ContainsAny(host, "\r\n\t ") {
		return nil, ErrUnsafeURL
	}
	return u, nil
}

func (c *Client) RequestMediaUploadURL(ctx context.Context, plan *MediaUploadPlan) error {
	if c.creds.BotToken == "" {
		return ErrCredentialsMissing
	}
	if err := validMediaPlan(plan); err != nil {
		return err
	}
	if plan.UploadURL != "" {
		return ErrInvalidRequest // the caller must not silently allocate another slot
	}
	mediaType, _ := mediaUploadType(plan.Type)
	var response struct {
		FullURL     string `json:"upload_full_url"`
		UploadParam string `json:"upload_param"`
		Ret         int    `json:"ret"`
		ErrCode     int    `json:"errcode"`
	}
	err := c.do(ctx, c.base, http.MethodPost, "ilink/bot/getuploadurl", map[string]any{
		"filekey": plan.FileKey, "media_type": mediaType, "to_user_id": plan.ToUserID,
		"rawsize": plan.RawSize, "rawfilemd5": plan.MD5, "filesize": plan.CipherSize,
		"no_need_thumb": true, "aeskey": plan.AESKeyHex, "base_info": c.baseInfo(),
	}, c.creds.BotToken, c.config.RequestTimeout, true, &response)
	if err != nil {
		return err
	}
	if err := businessError("getuploadurl", response.Ret, response.ErrCode); err != nil {
		return err
	}
	target := strings.TrimSpace(response.FullURL)
	if target == "" {
		if response.UploadParam == "" {
			return &APIError{Op: "getuploadurl", Kind: ErrOutcomeUnknown}
		}
		query := url.Values{"encrypted_query_param": {response.UploadParam}, "filekey": {plan.FileKey}}
		target = DefaultCDNURL + "/upload?" + query.Encode()
	}
	if _, err := trustedMediaURL(target); err != nil {
		return &APIError{Op: "getuploadurl", Kind: ErrUnsafeURL}
	}
	plan.UploadURL = target
	return nil
}

func (c *Client) mediaHTTP() *http.Client {
	client := *c.http
	client.Timeout = 0
	return &client
}

func mediaContext(ctx context.Context, opts MediaOptions) (context.Context, context.CancelFunc, error) {
	if opts.TransferTimeout < 0 {
		return nil, nil, ErrInvalidRequest
	}
	if opts.TransferTimeout > 0 {
		ctx, cancel := context.WithTimeout(ctx, opts.TransferTimeout)
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return ctx, cancel, nil
}

func transferError(ctx context.Context, op string, kind error, status int) error {
	return &APIError{Op: op, Kind: kind, HTTPStatus: status, cause: ctx.Err()}
}

// UploadPreparedMedia attempts one POST of the previously prepared ciphertext.
// The caller must journal upload_attempting BEFORE calling. Any uncertain result
// retains CipherPath and must not be blindly reuploaded, even with the same URL.
func (c *Client) UploadPreparedMedia(ctx context.Context, plan *MediaUploadPlan, opts MediaOptions) (*UploadedMedia, error) {
	if err := validMediaPlan(plan); err != nil {
		return nil, err
	}
	target, err := trustedMediaURL(plan.UploadURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel, err := mediaContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	file, err := os.Open(plan.CipherPath)
	if err != nil {
		return nil, errors.New("Weixin media snapshot unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != plan.CipherSize {
		return nil, ErrMediaIntegrity
	}
	digest := sha256.New()
	if _, err = io.CopyBuffer(digest, &mediaContextReader{ctx: ctx, source: file}, make([]byte, 32768)); err != nil {
		return nil, transferError(ctx, "cdn-upload-preflight", ErrMediaIntegrity, 0)
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), plan.CipherSHA256) {
		return nil, ErrMediaIntegrity
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrMediaIntegrity
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), file)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	req.ContentLength = plan.CipherSize
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.mediaHTTP().Do(req)
	if err != nil {
		return nil, transferError(ctx, "cdn-upload", ErrOutcomeUnknown, 0)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		kind := ErrAPI
		if response.StatusCode < 400 || response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
			kind = ErrOutcomeUnknown
		}
		return nil, transferError(ctx, "cdn-upload", kind, response.StatusCode)
	}
	param := response.Header.Get("x-encrypted-param")
	if param == "" {
		return nil, transferError(ctx, "cdn-upload", ErrOutcomeUnknown, response.StatusCode)
	}
	return &UploadedMedia{
		FileKey: plan.FileKey, DownloadParam: param, AESKeyHex: plan.AESKeyHex,
		MD5: plan.MD5, SHA256: plan.SHA256, Name: plan.Name, MIME: plan.MIME,
		ToUserID: plan.ToUserID, RawSize: plan.RawSize, CipherSize: plan.CipherSize, Type: plan.Type,
	}, nil
}

func BuildMediaItem(media UploadedMedia) (MessageItem, error) {
	if _, err := mediaUploadType(media.Type); err != nil {
		return MessageItem{}, err
	}
	if media.DownloadParam == "" || media.RawSize < 0 || media.CipherSize <= 0 || media.CipherSize%16 != 0 || media.CipherSize/16 != media.RawSize/16+1 {
		return MessageItem{}, ErrMediaIntegrity
	}
	if key, err := hex.DecodeString(media.AESKeyHex); err != nil || len(key) != 16 {
		return MessageItem{}, ErrMediaIntegrity
	}
	ref := cdnReference{EncryptQueryParam: media.DownloadParam, AESKey: base64.StdEncoding.EncodeToString([]byte(media.AESKeyHex)), EncryptType: 1}
	item := MessageItem{Type: media.Type}
	switch media.Type {
	case FileItemType:
		item.FileItem, _ = json.Marshal(map[string]any{"media": ref, "file_name": cleanMediaName(media.Name, "file.bin"), "len": strconv.FormatInt(media.RawSize, 10)})
	case ImageItemType:
		item.ImageItem, _ = json.Marshal(map[string]any{"media": ref, "mid_size": media.CipherSize})
	case VideoItemType:
		item.VideoItem, _ = json.Marshal(map[string]any{"media": ref, "video_size": media.CipherSize})
	}
	return item, nil
}

func (c *Client) SendMedia(ctx context.Context, input SendMediaRequest) (*SendResult, error) {
	if c.creds.BotToken == "" {
		return nil, ErrCredentialsMissing
	}
	if input.ContextToken == "" {
		return nil, ErrContextRequired
	}
	if input.ToUserID == "" || input.ClientID == "" || input.Uploaded.ToUserID != input.ToUserID {
		return nil, ErrInvalidRequest
	}
	item, err := BuildMediaItem(input.Uploaded)
	if err != nil {
		return nil, err
	}
	message := map[string]any{
		"from_user_id": "", "to_user_id": input.ToUserID, "client_id": input.ClientID,
		"message_type": BotMessage, "message_state": FinishedState,
		"context_token": input.ContextToken, "item_list": []MessageItem{item},
	}
	if input.RunID != "" {
		message["run_id"] = input.RunID
	}
	var response struct {
		MessageID ID  `json:"message_id"`
		Ret       int `json:"ret"`
		ErrCode   int `json:"errcode"`
	}
	err = c.do(ctx, c.base, http.MethodPost, "ilink/bot/sendmessage", map[string]any{"msg": message, "base_info": c.baseInfo()}, c.creds.BotToken, c.config.RequestTimeout, true, &response)
	if err != nil {
		return nil, err
	}
	if err = businessError("sendmessage", response.Ret, response.ErrCode); err != nil {
		return nil, err
	}
	if response.MessageID == "" {
		return nil, &APIError{Op: "sendmessage", Kind: ErrOutcomeUnknown}
	}
	return &SendResult{MessageID: response.MessageID, ClientID: input.ClientID}, nil
}

func decodeInboundMedia(item MessageItem) (*inboundMedia, []byte, error) {
	var raw json.RawMessage
	switch item.Type {
	case ImageItemType:
		raw = item.ImageItem
	case FileItemType:
		raw = item.FileItem
	case VideoItemType:
		raw = item.VideoItem
	case VoiceItemType:
		raw = item.VoiceItem
	default:
		return nil, nil, ErrMediaUnsupported
	}
	var media inboundMedia
	if len(raw) == 0 || json.Unmarshal(raw, &media) != nil || media.Media == nil {
		return nil, nil, ErrMediaIntegrity
	}
	media.Type = item.Type
	if item.Type == VoiceItemType && (media.EncodeType < 0 || media.BitsPerSample < 0 || media.SampleRate < 0 || media.PlaytimeMS < 0) {
		return nil, nil, ErrMediaIntegrity
	}
	if media.Media.EncryptType != 0 && media.Media.EncryptType != 1 {
		return nil, nil, ErrMediaUnsupported
	}
	if media.Length != "" && item.Type == FileItemType {
		size, err := strconv.ParseInt(media.Length, 10, 64)
		if err != nil || size < 0 {
			return nil, nil, ErrMediaIntegrity
		}
		media.RawSize = &size
	}
	if media.MD5 != "" {
		if digest, err := hex.DecodeString(media.MD5); err != nil || len(digest) != 16 {
			return nil, nil, ErrMediaIntegrity
		}
	}
	var key []byte
	var err error
	if item.Type == ImageItemType && media.AESKey != "" {
		key, err = hex.DecodeString(media.AESKey)
	} else if media.Media.AESKey != "" {
		key, err = base64.StdEncoding.DecodeString(media.Media.AESKey)
		if err == nil && len(key) == 32 {
			key, err = hex.DecodeString(string(key))
		}
	} else if item.Type != ImageItemType {
		return nil, nil, ErrMediaIntegrity
	}
	if err != nil || (key != nil && len(key) != 16) {
		return nil, nil, ErrMediaIntegrity
	}
	return &media, key, nil
}

// DownloadMedia fetches and verifies the referenced media bytes, then returns a private
// local file. A failed transfer removes its partial file; successful file
// ownership transfers to the caller. No thumbnail is substituted for an original.
// Any compression already performed by Weixin cannot be reversed here.
func (c *Client) DownloadMedia(ctx context.Context, item MessageItem, opts MediaOptions) (*MediaFile, error) {
	media, key, err := decodeInboundMedia(item)
	if err != nil {
		return nil, err
	}
	target := media.Media.FullURL
	if target == "" {
		if media.Media.EncryptQueryParam == "" {
			return nil, ErrMediaIntegrity
		}
		target = DefaultCDNURL + "/download?" + url.Values{"encrypted_query_param": {media.Media.EncryptQueryParam}}.Encode()
	}
	u, err := trustedMediaURL(target)
	if err != nil {
		return nil, err
	}
	ctx, cancel, err := mediaContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cancel()
	output, err := privateMediaTemp(opts.TempDir, "weixin-original-*")
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		output.Close()
		if !completed {
			os.Remove(output.Name())
		}
	}()
	original, cipherSize, err := c.downloadMediaOriginal(ctx, u, opts, media.MD5 != "")
	if err != nil {
		return nil, err
	}
	defer func() {
		original.Close()
		os.Remove(original.Name())
	}()
	plainMD5, plainSHA := md5.New(), sha256.New()
	writer := io.MultiWriter(output, plainMD5, plainSHA)
	reader := &mediaCountReader{source: &mediaContextReader{ctx: ctx, source: original}}
	var size int64
	if key != nil {
		size, err = decryptECB(ctx, reader, writer, key)
	} else {
		size, err = io.CopyBuffer(writer, reader, make([]byte, 32768))
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, mediaReadFailure(ctx, "cancelled", 0, reader.count, cipherSize, false)
		}
		var readError *mediaCryptoReadError
		if errors.As(err, &readError) {
			return nil, mediaReadFailure(ctx, "local-read", 0, reader.count, cipherSize, false)
		}
		return nil, ErrMediaIntegrity
	}
	digestMD5 := hex.EncodeToString(plainMD5.Sum(nil))
	if reader.count != cipherSize ||
		(media.RawSize != nil && size != *media.RawSize) || (media.MD5 != "" && !strings.EqualFold(media.MD5, digestMD5)) {
		return nil, ErrMediaIntegrity
	}
	if err = output.Sync(); err != nil {
		return nil, errors.New("cannot persist Weixin media original")
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("cannot inspect Weixin media original")
	}
	var prefix [512]byte
	n, err := output.Read(prefix[:])
	if err != nil && err != io.EOF {
		return nil, ErrMediaIntegrity
	}
	contentType := http.DetectContentType(prefix[:n])
	var voice *VoiceFormat
	if item.Type == VoiceItemType {
		voice = &VoiceFormat{EncodeType: media.EncodeType, BitsPerSample: media.BitsPerSample, SampleRate: media.SampleRate, PlaytimeMS: media.PlaytimeMS}
		name, voiceMIME := voiceFileFormat(prefix[:n], voice)
		media.Name, contentType = name, voiceMIME
	}
	name := cleanMediaName(media.Name, "")
	if name == "" {
		name = "image.bin"
		if item.Type == VideoItemType {
			name = "video.mp4"
		} else if item.Type == FileItemType {
			name = "file.bin"
		}
		if item.Type == ImageItemType {
			for mediaMIME, ext := range map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp"} {
				if contentType == mediaMIME {
					name = "image" + ext
				}
			}
		}
	}
	if item.Type == FileItemType {
		if extMIME := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); extMIME != "" {
			contentType = extMIME
		}
	}
	if err = output.Close(); err != nil {
		return nil, errors.New("cannot close Weixin media original")
	}
	completed = true
	return &MediaFile{Path: output.Name(), Name: name, MIME: contentType, MD5: digestMD5, SHA256: fmt.Sprintf("%x", plainSHA.Sum(nil)), Size: size, Type: item.Type, Voice: voice}, nil
}

// A raw codec declaration does not establish an Ogg/WAV container. Preserve
// bytes exactly; conversion, if needed by the destination, is a separate step.
func voiceFileFormat(prefix []byte, format *VoiceFormat) (name, contentType string) {
	format.Container = "raw"
	switch format.EncodeType {
	case 1:
		format.Codec = "pcm"
	case 2:
		format.Codec = "adpcm"
	case 3:
		format.Codec = "feature"
	case 4:
		format.Codec = "speex"
	case 5:
		format.Codec = "amr"
	case 6:
		format.Codec = "silk"
	case 7:
		format.Codec = "mp3"
	case 8:
		format.Codec = "ogg-speex"
	default:
		format.Codec = "unknown"
	}
	format.DeclaredCodec = format.Codec
	switch {
	case len(prefix) >= 12 && bytes.Equal(prefix[:4], []byte("RIFF")) && bytes.Equal(prefix[8:12], []byte("WAVE")):
		format.Container = "wav"
		return "voice.wav", "audio/wav"
	case bytes.HasPrefix(prefix, []byte("OggS")):
		format.Container = "ogg"
		return "voice.ogg", "audio/ogg"
	case bytes.HasPrefix(prefix, []byte("#!AMR-WB\n")):
		format.Codec = "amr"
		format.Container = "amr-wb"
		return "voice.awb", "audio/amr-wb"
	case bytes.HasPrefix(prefix, []byte("#!AMR\n")):
		format.Codec = "amr"
		format.Container = "amr"
		return "voice.amr", "audio/amr"
	case bytes.HasPrefix(prefix, []byte("#!SILK_V3")) || bytes.HasPrefix(prefix, append([]byte{2}, []byte("#!SILK_V3")...)):
		format.Codec = "silk"
		format.Container = "silk"
		return "voice.silk", "audio/silk"
	case bytes.HasPrefix(prefix, []byte("ID3")):
		format.Codec = "mp3"
		format.Container = "mp3"
		return "voice.mp3", "audio/mpeg"
	}
	// The source explicitly labels these raw codecs, but provides no container
	// proof. In particular, raw Speex must never be renamed to an Ogg file.
	switch format.EncodeType {
	case 1:
		return "voice.pcm", "application/octet-stream"
	case 4:
		return "voice.speex", "audio/speex"
	case 6:
		return "voice.silk", "audio/silk"
	default:
		return "voice.raw", "application/octet-stream"
	}
}
