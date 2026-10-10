package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

type FileReference struct {
	Type   string `json:"type"`
	FileID string `json:"file_id"`
}

// UploadOutcomeUnknownError means the server may have stored the file, but no
// reliable file ID was received. Callers must not automatically repeat it: this
// endpoint has no known idempotency or upload reconciliation contract.
type UploadOutcomeUnknownError struct{ Cause error }

func (e *UploadOutcomeUnknownError) Error() string {
	return "attachment upload outcome unknown; do not automatically retry"
}
func (e *UploadOutcomeUnknownError) Unwrap() error { return e.Cause }

// UploadRejectedError describes an explicit HTTP rejection, not an unknown
// upload outcome. Only fixed categories and an explicitly stated size limit
// leave the response parser. A limit retains its original unit; MB is not MiB.
type UploadRejectedError struct {
	HTTPStatus int
	Reason     string
	LimitValue string
	LimitUnit  string
}

func (e *UploadRejectedError) Error() string {
	text := fmt.Sprintf("attachment upload rejected: http=%d reason=%s", e.HTTPStatus, e.Reason)
	if e.LimitValue != "" && e.LimitUnit != "" {
		text += fmt.Sprintf(" limit=%s %s", e.LimitValue, e.LimitUnit)
	}
	return text
}
func (e *UploadRejectedError) Unwrap() error { return &HTTPError{Status: e.HTTPStatus} }

// This bounds diagnostic JSON only, never the uploaded file. Unknown fields,
// arbitrary error codes, server text, and incomplete/oversized bodies are not
// retained or printed.
const uploadErrorMetadataBytes = 16 << 10

var uploadSizeContext = regexp.MustCompile(`(?i)\b(?:files?|attachments?|uploads?|payload|request)\b|文件|附件|上传`)
var uploadTooLargeMessage = regexp.MustCompile(`(?i)\b(?:file|attachment|payload|request)\s+(?:(?:size\s+)?(?:is|was)\s+)?too\s+large\b|\b(?:file|attachment|payload|request)\s+size\s+exceed|文件过大|文件大小超`)
var uploadSizePolicyMessage = regexp.MustCompile(`(?i)^(?:the\s+)?(?:max(?:imum)?(?:\s+(?:allowed|permitted|supported))?(?:\s+(?:file|attachment|upload|payload|request))?\s+size|(?:file|attachment|upload|payload|request)\s+size\s+(?:limit|must\s+not\s+exceed|cannot\s+exceed))\b|^(?:最大(?:文件|附件|上传)?(?:大小|尺寸)|(?:文件|附件)(?:大小)?(?:上限|不能超过|不得超过)|文件大小限制)`)

var uploadSizeLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:max(?:imum)?(?:\s+(?:allowed|permitted|supported))?(?:\s+(?:file|attachment|upload|payload|request))?\s+size(?:\s+limit)?|(?:file|attachment|upload|payload|request)\s+size\s+limit)(?:\s+(?:is|of))?\s*[:=]?\s*([0-9]{1,19}(?:\.[0-9]{1,6})?)\s*(bytes?|[kmgt]i?b|B)\b`),
	regexp.MustCompile(`(?i)\b(?:smaller\s+than|less\s+than|must\s+be\s+(?:under|below)|must\s+not\s+exceed|cannot\s+exceed|can\s+not\s+exceed)\s*([0-9]{1,19}(?:\.[0-9]{1,6})?)\s*(bytes?|[kmgt]i?b|B)\b`),
	regexp.MustCompile(`(?:最大(?:文件|附件|上传)?(?:大小|尺寸)(?:限制)?|(?:文件|附件)(?:大小)?(?:上限|不能超过|不得超过)|文件大小限制)(?:为|是)?\s*[:：=]?\s*([0-9]{1,19}(?:\.[0-9]{1,6})?)\s*(bytes?|[KMGT]i?B|B)\b`),
}

func uploadDiagnosticFields(raw json.RawMessage, depth int) (codes, messages []string) {
	if depth > 3 {
		return nil, nil
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return nil, []string{message}
	}
	var fields struct {
		Code    string          `json:"code"`
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &fields) == nil {
		codes = []string{fields.Code, fields.Type}
		messages = []string{fields.Message, fields.Msg}
		for _, nested := range []json.RawMessage{fields.Detail, fields.Error} {
			childCodes, childMessages := uploadDiagnosticFields(nested, depth+1)
			codes, messages = append(codes, childCodes...), append(messages, childMessages...)
		}
		return codes, messages
	}
	var details []json.RawMessage
	if json.Unmarshal(raw, &details) == nil {
		for _, detail := range details {
			childCodes, childMessages := uploadDiagnosticFields(detail, depth+1)
			codes, messages = append(codes, childCodes...), append(messages, childMessages...)
		}
	}
	return codes, messages
}

func uploadRejection(status int, body io.Reader) *UploadRejectedError {
	rejected := &UploadRejectedError{HTTPStatus: status, Reason: "unknown"}
	switch status {
	case http.StatusRequestEntityTooLarge:
		rejected.Reason = "payload-too-large"
	case http.StatusUnsupportedMediaType:
		rejected.Reason = "unsupported-media-type"
	case http.StatusUnauthorized:
		rejected.Reason = "authentication-required"
	case http.StatusForbidden:
		rejected.Reason = "permission-denied"
	case http.StatusTooManyRequests:
		rejected.Reason = "rate-limited"
	}
	raw, err := io.ReadAll(io.LimitReader(body, uploadErrorMetadataBytes+1))
	if err != nil || len(raw) > uploadErrorMetadataBytes || !json.Valid(raw) {
		return rejected
	}
	codes, messages := uploadDiagnosticFields(raw, 0)
	// Status-derived authentication/permission/quota failures must not become
	// size failures merely because their text mentions an attachment limit.
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusRequestEntityTooLarge {
		return rejected
	}
	for _, code := range codes {
		switch strings.ToLower(strings.TrimSpace(code)) {
		case "file_too_large", "file_size_exceeded", "file_size_limit_exceeded", "max_file_size_exceeded", "payload_too_large", "request_entity_too_large":
			rejected.Reason = "payload-too-large"
		case "unsupported_file_type", "unsupported_media_type":
			if rejected.Reason == "unknown" {
				rejected.Reason = "unsupported-media-type"
			}
		}
	}
	conflictingLimit := false
	for _, message := range messages {
		if !uploadSizeContext.MatchString(message) {
			continue
		}
		sizeMessage := uploadTooLargeMessage.MatchString(message)
		if sizeMessage {
			rejected.Reason = "payload-too-large"
		}
		if rejected.Reason != "payload-too-large" && !uploadSizePolicyMessage.MatchString(message) {
			continue // Do not treat arbitrary echoed names/URLs as a size policy.
		}
		for _, pattern := range uploadSizeLimitPatterns {
			for _, match := range pattern.FindAllStringSubmatch(message, -1) {
				value, unit := match[1], match[2]
				if rejected.LimitValue != "" && (rejected.LimitValue != value || !strings.EqualFold(rejected.LimitUnit, unit)) {
					conflictingLimit = true
				}
				rejected.Reason = "payload-too-large"
				rejected.LimitValue, rejected.LimitUnit = value, unit
			}
		}
	}
	if conflictingLimit {
		rejected.LimitValue, rejected.LimitUnit = "", ""
	}
	return rejected
}

type multipartPayload struct {
	prefix, suffix []byte
	contentType    string
	size           int64
	open           func() io.Reader
}

// Upload retains the upstream byte-slice API. It streams the multipart envelope
// around the original bytes without making another file-sized copy. New bridge
// code should use UploadFile instead.
func (c *Client) Upload(ctx context.Context, room, filename, contentType string, data []byte) (string, error) {
	payload, err := prepareMultipart(filename, contentType, int64(len(data)), func() io.Reader {
		return bytes.NewReader(data)
	})
	if err != nil {
		return "", err
	}
	return c.upload(ctx, room, payload)
}

// UploadFile streams a regular local file. Keep its bytes unchanged until this
// method returns. The opened file is closed on every return path. Context
// cancellation/deadlines control the transfer; media has no 30-second total cap.
func (c *Client) UploadFile(ctx context.Context, room, filename, contentType, localPath string) (string, error) {
	if err := c.verifyUploadRoom(room); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(localPath)
	if err != nil {
		return "", errors.New("cannot open local attachment")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return "", errors.New("attachment source must be a readable regular file")
	}
	payload, err := prepareMultipart(filename, contentType, stat.Size(), func() io.Reader {
		// Retain the same opened file instead of reopening a changed path after
		// a known 401 rejection. Each attempt starts with a fresh read cursor.
		return io.NewSectionReader(file, 0, stat.Size())
	})
	if err != nil {
		return "", err
	}
	return c.upload(ctx, room, payload)
}

func prepareMultipart(filename, contentType string, size int64, open func() io.Reader) (*multipartPayload, error) {
	filename = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	if filename == "" {
		filename = "attachment"
	}
	if len(filename) > 255 {
		return nil, errors.New("attachment filename exceeds 255 bytes")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filename))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errors.New("invalid attachment MIME type")
	}
	var envelope bytes.Buffer
	writer := multipart.NewWriter(&envelope)
	headers := textproto.MIMEHeader{}
	// Match multipart.CreateFormFile's quoted UTF-8 parameter. FormatMediaType
	// emits filename* for non-ASCII names, which multipart/form-data forbids.
	quotedFilename := strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(filename)
	headers.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quotedFilename))
	headers.Set("Content-Type", parsed)
	if _, err = writer.CreatePart(headers); err != nil {
		return nil, errors.New("cannot prepare attachment upload")
	}
	prefix := bytes.Clone(envelope.Bytes())
	envelope.Reset()
	if err = writer.Close(); err != nil {
		return nil, errors.New("cannot prepare attachment upload")
	}
	suffix := bytes.Clone(envelope.Bytes())
	if size < 0 || size > math.MaxInt64-int64(len(prefix))-int64(len(suffix)) {
		return nil, errors.New("attachment size cannot be represented by HTTP")
	}
	return &multipartPayload{prefix: prefix, suffix: suffix, contentType: writer.FormDataContentType(), size: size, open: open}, nil
}

func (c *Client) verifyUploadRoom(room string) error {
	c.mu.RLock()
	verified := c.rooms[room]
	c.mu.RUnlock()
	if !verified {
		return errors.New("refusing unverified Dot room")
	}
	return nil
}

// Take an auth snapshot without keeping authMu locked during a long upload.
// Ordinary room polling and messages must remain usable during media transfer.
func (c *Client) uploadAuth(ctx context.Context, rejectedToken string) (Credentials, string, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if time.Until(c.Identity.Expires) < 5*time.Minute || (rejectedToken != "" && rejectedToken == c.credentials.AccessToken) {
		if err := c.refresh(ctx); err != nil {
			return Credentials{}, "", err
		}
	}
	return c.credentials, c.Identity.Account, nil
}

func (c *Client) upload(ctx context.Context, room string, payload *multipartPayload) (string, error) {
	if err := c.verifyUploadRoom(room); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	credentials, account, err := c.uploadAuth(ctx, "")
	if err != nil {
		return "", err
	}
	id, err := c.uploadAttempt(ctx, room, payload, credentials, account)
	var failure *HTTPError
	if !errors.As(err, &failure) || failure.Status != http.StatusUnauthorized {
		return id, err
	}
	// Only an explicit authentication rejection is replayed. Transport errors,
	// cancellation after starting, 5xx and malformed successes remain unknown.
	credentials, account, err = c.uploadAuth(ctx, credentials.AccessToken)
	if err != nil {
		return "", err
	}
	return c.uploadAttempt(ctx, room, payload, credentials, account)
}

func (c *Client) uploadAttempt(ctx context.Context, room string, payload *multipartPayload, credentials Credentials, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body := io.MultiReader(bytes.NewReader(payload.prefix), payload.open(), bytes.NewReader(payload.suffix))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/messaging/rooms/"+url.PathEscape(room)+"/files", io.NopCloser(body))
	if err != nil {
		return "", errors.New("cannot prepare attachment upload request")
	}
	req.ContentLength = int64(len(payload.prefix)) + payload.size + int64(len(payload.suffix))
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", account)
	req.Header.Set("X-OpenAI-Expected-Account-Id", account)
	req.Header.Set("Content-Type", payload.contentType)
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	resp, err := c.mediaHTTP.Do(req)
	if err != nil {
		cause := ctx.Err()
		if cause == nil {
			cause = errors.New("attachment upload transport failed")
		}
		return "", &UploadOutcomeUnknownError{Cause: cause}
	}
	defer resp.Body.Close()
	// A timeout or redirect response does not prove that the file was never
	// stored. This endpoint has no verified upload reconciliation contract.
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		return "", &UploadOutcomeUnknownError{Cause: &HTTPError{Status: resp.StatusCode}}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", uploadRejection(resp.StatusCode, resp.Body)
		}
		return "", &HTTPError{Status: resp.StatusCode}
	}
	var response struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&response) != nil || response.ID == "" {
		return "", &UploadOutcomeUnknownError{Cause: errors.New("attachment upload response has no reliable file identity")}
	}
	return response.ID, nil
}
