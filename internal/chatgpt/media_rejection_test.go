package chatgpt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUploadRejectionRetainsOnlyExplicitSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason, value, unit string
		status                          int
	}{
		{"detail-size", `{"detail":"File size exceeds maximum allowed size of 100 MB. https://secret.example/private-token"}`, "payload-too-large", "100", "MB", 400},
		{"error-size", `{"error":{"message":"The file is too large. Please upload a file smaller than 100 MiB.","code":"invalid_request_error"}}`, "payload-too-large", "100", "MiB", 400},
		{"nested-detail", `{"detail":{"code":"file_size_exceeded","message":"Maximum file size: 104857600 bytes","input":"private-token"}}`, "payload-too-large", "104857600", "bytes", 422},
		{"decimal-policy", `{"detail":"Maximum file size is 0.5 GB"}`, "payload-too-large", "0.5", "GB", 400},
		{"size-no-limit", `{"error":{"code":"file_too_large","message":"File is too large"}}`, "payload-too-large", "", "", 400},
		{"supplied-size-is-not-limit", `{"detail":"File size is 131868123 bytes"}`, "unknown", "", "", 400},
		{"size-rejection-supplied-size", `{"detail":"File size exceeds 131868123 bytes"}`, "payload-too-large", "", "", 400},
		{"numeric-field-no-unit", `{"error":{"code":"file_too_large","max_file_size":104857600}}`, "payload-too-large", "", "", 400},
		{"unknown-code", `{"error":{"code":"private-token","message":"Invalid file https://secret.example/private-token"}}`, "unknown", "", "", 400},
		{"echoed-filename-is-not-limit", `{"detail":"Invalid file name: maximum size 100MB.zip"}`, "unknown", "", "", 400},
		{"quota-is-not-file-limit", `{"detail":"Upload daily quota is 100 MB"}`, "unknown", "", "", 400},
		{"conflicting-limits", `{"error":{"code":"file_too_large","message":"Maximum file size: 100 MB. Maximum file size: 50 MB."}}`, "payload-too-large", "", "", 400},
		{"413-no-limit", `<html>private-token</html>`, "payload-too-large", "", "", 413},
		{"400-html-not-metadata", `<html>Maximum file size 100MB private-token</html>`, "unknown", "", "", 400},
		{"malformed", `{"detail":"Maximum file size 100 MB private-token"`, "unknown", "", "", 400},
		{"unsupported", `{"error":{"code":"unsupported_file_type","message":"private-token"}}`, "unsupported-media-type", "", "", 400},
		{"415", `{}`, "unsupported-media-type", "", "", 415},
		{"403-not-size", `{"detail":"Maximum file size 100 MB private-token"}`, "permission-denied", "", "", 403},
		{"429-not-size", `{"detail":"Maximum file size 100 MB private-token"}`, "rate-limited", "", "", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := &mediaTrackedBody{Reader: strings.NewReader(tc.body)}
			c := mediaTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls++
				response := mediaResponse(req, tc.status, "")
				response.Body = body
				return response, nil
			})
			id, err := c.Upload(context.Background(), "room", "f.zip", "application/zip", []byte("source"))
			var rejected *UploadRejectedError
			var httpErr *HTTPError
			var unknown *UploadOutcomeUnknownError
			if id != "" || calls != 1 || !body.closed || !errors.As(err, &rejected) || !errors.As(err, &httpErr) || errors.As(err, &unknown) ||
				rejected.HTTPStatus != tc.status || httpErr.Status != tc.status || rejected.Reason != tc.reason || rejected.LimitValue != tc.value || rejected.LimitUnit != tc.unit {
				t.Fatalf("wrong explicit rejection: id=%q calls=%d rejected=%+v err=%v", id, calls, rejected, err)
			}
			if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), tc.body) {
				t.Fatal("untrusted server body escaped into a diagnostic")
			}
		})
	}
}

type uploadDiagnosticFailReader struct{}

func (uploadDiagnosticFailReader) Read(p []byte) (int, error) {
	n := copy(p, `{"detail":"File is too large. Maximum file size 100 MB"}`)
	return n, errors.New("https://secret.example/private-token")
}

func TestUploadDiagnosticBodyIsBoundedAndReadFailureStaysSafe(t *testing.T) {
	for _, body := range []io.Reader{
		strings.NewReader(`{"detail":"File is too large. Maximum file size 100 MB. ` + strings.Repeat("private-token", uploadErrorMetadataBytes) + `"}`),
		uploadDiagnosticFailReader{},
	} {
		err := uploadRejection(400, body)
		if err.Reason != "unknown" || err.LimitValue != "" || err.LimitUnit != "" || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("oversized/failed diagnostic body was trusted or disclosed: %v", err)
		}
	}
}
