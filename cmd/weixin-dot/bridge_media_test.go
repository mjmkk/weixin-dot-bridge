// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

type bridgeZeroReader struct{}

func (bridgeZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func bridgeMediaFixture(dir, name, mime string, size int64, kind int) (*weixin.MediaFile, error) {
	f, err := os.CreateTemp(dir, "fixture-*")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sha, md := sha256.New(), md5.New()
	if _, err = io.CopyN(io.MultiWriter(f, sha, md), bridgeZeroReader{}, size); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return &weixin.MediaFile{Path: f.Name(), Name: name, MIME: mime, Size: size, Type: kind, SHA256: hex.EncodeToString(sha.Sum(nil)), MD5: hex.EncodeToString(md.Sum(nil))}, nil
}

type bridgeMediaNativeFake struct {
	bridgeNativeFake
	uploads      int
	uploadErr    error
	downloads    []string
	downloadSize int64
	downloadMIME string
	downloadErr  error
	sentText     string
	sentRefs     []chatgpt.FileReference
	beforeUpload func() error
}

func (f *bridgeMediaNativeFake) UploadFile(_ context.Context, _, _, _, path string) (string, error) {
	if f.beforeUpload != nil {
		if err := f.beforeUpload(); err != nil {
			return "", err
		}
	}
	f.uploads++
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return "uploaded-native-file", nil
}
func (f *bridgeMediaNativeFake) DownloadAttachmentTo(_ context.Context, _, id string, dst io.Writer) (*chatgpt.FileInfo, error) {
	f.downloads = append(f.downloads, id)
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	_, err := io.CopyN(dst, bridgeZeroReader{}, f.downloadSize)
	if err != nil {
		return nil, err
	}
	mime := f.downloadMIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	return &chatgpt.FileInfo{Name: "original.bin", MIME: mime, Size: f.downloadSize}, nil
}
func (f *bridgeMediaNativeFake) Send(_ context.Context, _, text, request string, refs []chatgpt.FileReference) (*chatgpt.Message, error) {
	f.sends = append(f.sends, request)
	f.sentText = text
	f.sentRefs = append([]chatgpt.FileReference(nil), refs...)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	m := &chatgpt.Message{ID: "native-message", RequestID: request}
	m.Content.Text = text
	for _, ref := range refs {
		raw, _ := json.Marshal(ref)
		m.Content.Attachments = append(m.Content.Attachments, raw)
	}
	return m, nil
}

type bridgeMediaWeixinFake struct {
	bridgeWeixinFake
	downloading     chan struct{}
	release         chan struct{}
	downloadSize    int64
	slots           int
	uploads         int
	mediaSends      []weixin.SendMediaRequest
	slotErr         error
	uploadErr       error
	mediaSendErr    error
	beforeSlot      func(*weixin.MediaUploadPlan) error
	beforeUpload    func(*weixin.MediaUploadPlan) error
	beforeMediaSend func(weixin.SendMediaRequest)
}

func (f *bridgeMediaWeixinFake) DownloadMedia(ctx context.Context, item weixin.MessageItem, opts weixin.MediaOptions) (*weixin.MediaFile, error) {
	if f.downloading != nil {
		close(f.downloading)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	mime := "application/octet-stream"
	if item.Type == weixin.ImageItemType {
		mime = "image/png"
	} else if item.Type == weixin.VideoItemType {
		mime = "video/mp4"
	} else if item.Type == weixin.VoiceItemType {
		mime = "audio/silk"
	}
	return bridgeMediaFixture(opts.TempDir, "original.bin", mime, f.downloadSize, item.Type)
}
func (f *bridgeMediaWeixinFake) RequestMediaUploadURL(_ context.Context, plan *weixin.MediaUploadPlan) error {
	if f.beforeSlot != nil {
		if err := f.beforeSlot(plan); err != nil {
			return err
		}
	}
	f.slots++
	if f.slotErr != nil {
		return f.slotErr
	}
	plan.UploadURL = "https://novac2c.cdn.weixin.qq.com/c2c/upload?opaque=secret"
	return nil
}
func (f *bridgeMediaWeixinFake) UploadPreparedMedia(_ context.Context, plan *weixin.MediaUploadPlan, _ weixin.MediaOptions) (*weixin.UploadedMedia, error) {
	if f.beforeUpload != nil {
		if err := f.beforeUpload(plan); err != nil {
			return nil, err
		}
	}
	f.uploads++
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	return &weixin.UploadedMedia{FileKey: plan.FileKey, AESKeyHex: plan.AESKeyHex, MD5: plan.MD5, SHA256: plan.SHA256, Name: plan.Name, MIME: plan.MIME, ToUserID: plan.ToUserID, RawSize: plan.RawSize, CipherSize: plan.CipherSize, Type: plan.Type, DownloadParam: "private-download-param"}, nil
}
func (f *bridgeMediaWeixinFake) SendMedia(_ context.Context, in weixin.SendMediaRequest) (*weixin.SendResult, error) {
	if f.beforeMediaSend != nil {
		f.beforeMediaSend(in)
	}
	f.mediaSends = append(f.mediaSends, in)
	if f.mediaSendErr != nil {
		return nil, f.mediaSendErr
	}
	return &weixin.SendResult{MessageID: "wx-media-message", ClientID: in.ClientID}, nil
}

func bridgeTestMediaEngine(t *testing.T) (*bridgeEngine, *bridgeMediaNativeFake, *bridgeMediaWeixinFake) {
	t.Helper()
	e, _, _ := bridgeTestEngine()
	n, w := &bridgeMediaNativeFake{}, &bridgeMediaWeixinFake{}
	e.native = n
	e.weixin = w
	e.mediaDir = t.TempDir()
	if err := os.Chmod(e.mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	e.mediaResults = make(chan bridgeMediaResult, 1)
	e.state.ContextToken = "context"
	e.prepareMedia = weixin.PrepareMediaUpload
	return e, n, w
}

func bridgeTestMediaMessage(kind int) weixin.Message {
	m := bridgeTestText("media-inbound", "peer", "original instruction", "context")
	item := weixin.MessageItem{Type: kind}
	switch kind {
	case weixin.FileItemType:
		item.FileItem = json.RawMessage(`{"media":{"encrypted_query_param":"reference"},"file_name":"original.bin"}`)
	case weixin.ImageItemType:
		item.ImageItem = json.RawMessage(`{"media":{"encrypted_query_param":"reference"}}`)
	case weixin.VideoItemType:
		item.VideoItem = json.RawMessage(`{"media":{"encrypted_query_param":"reference"}}`)
	case weixin.VoiceItemType:
		item.VoiceItem = json.RawMessage(`{"media":{"encrypted_query_param":"reference"},"encode_type":4,"playtime":2440}`)
	}
	m.ItemList = append(m.ItemList, item)
	return m
}

func bridgeFinishMedia(t *testing.T, e *bridgeEngine) {
	t.Helper()
	select {
	case result := <-e.mediaResults:
		if err := e.acceptMedia(result); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("media worker did not complete")
	}
}

func TestBridgeMediaIncomingOriginalsToSameRoom(t *testing.T) {
	for _, kind := range []int{weixin.FileItemType, weixin.ImageItemType, weixin.VideoItemType, weixin.VoiceItemType} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			w.downloadSize = 32
			if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(kind)}, Cursor: "new"}); err != nil {
				t.Fatal(err)
			}
			var durable *bridgeState
			e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
			n.beforeUpload = func() error {
				if durable.Inbound[0].Media[0].Stage != "native_upload_attempting" || durable.Inbound[0].Media[0].File.SHA256 == "" {
					return errors.New("upload without durable original")
				}
				return nil
			}
			for phase := 0; phase < 2; phase++ {
				if err := e.deliverNative(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			if err := e.deliverNative(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n.uploads != 1 || n.sentText != "original instruction" || len(n.sentRefs) != 1 || n.sentRefs[0].FileID != "uploaded-native-file" || e.state.Inbound[0].Status != "acknowledged" {
				t.Fatal("original instruction and uploaded attachment did not enter one native message")
			}
			media := e.state.Inbound[0].Media[0]
			if !media.Cleaned || media.File.Size != 32 || media.File.SHA256 == "" {
				t.Fatal("confirmed media lost byte receipt or was not cleaned")
			}
			if _, err := os.Stat(media.File.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("confirmed original was not removed")
			}
		})
	}
}

func TestBridgeLargeLocalFileHasNoFormerTwentyMiBGate(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	w.downloadSize = 21 << 20
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 2; phase++ {
		if err := e.deliverNative(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.uploads != 1 || e.state.Inbound[0].Media[0].File.Size != 21<<20 || e.state.Inbound[0].Status != "acknowledged" {
		t.Fatal("former local size gate remains")
	}
}

func TestBridgeInterruptedDownloadRetainsRetryAndPreciseNotice(t *testing.T) {
	e, n, _ := bridgeTestMediaEngine(t)
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	media := e.state.Inbound[0].Media[0]
	media.Stage, media.Attempts = "downloading", 1
	job := bridgeMediaJob{ID: media.ID, Direction: "in", Phase: "downloading"}
	e.mediaBusy = &job
	err := &weixin.MediaReadError{Reason: "read-interrupted", HTTPStatus: 200, BytesRead: 32776208, ExpectedBytes: 131868128, Retryable: true}
	if err := e.acceptMedia(bridgeMediaResult{Job: job, Err: err}); err != nil {
		t.Fatal(err)
	}
	if media.Stage != "received" || !media.RetryAt.After(time.Now()) || media.ReadFailure == nil || media.ReadFailure.BytesRead != 32776208 || n.uploads != 0 || len(n.sends) != 0 {
		t.Fatal("interrupted GET was not retained for safe retry, or reached native POST")
	}
	if len(e.state.Outbound) != 1 {
		t.Fatal("expected a precise download notice")
	}
	notice := e.state.Outbound[0].Text
	if !strings.Contains(notice, "32776208 / 131868128") || !strings.Contains(notice, "下载连接已中断") || !strings.Contains(notice, "尚未发送到 dot") || strings.Contains(notice, "拒绝") {
		t.Fatalf("download interruption notice misattributes the failure: %s", notice)
	}
	if !bridgeIncomingRecordsValid(e.state.Inbound[0].Media) {
		t.Fatal("retry journal is invalid")
	}
}

func TestBridgeUploadRejectionKeepsItsPhaseAndProviderLimit(t *testing.T) {
	e, _, _ := bridgeTestMediaEngine(t)
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	media := e.state.Inbound[0].Media[0]
	if err := e.mediaNotice(media.ID, "old download failure"); err != nil {
		t.Fatal(err)
	}
	e.state.Outbound[0].Status = "acknowledged"
	e.state.Outbound[0].WeixinID = "old-ack"
	media.File, _ = bridgeMediaFixture(e.mediaDir, "original.zip", "application/zip", 32, weixin.FileItemType)
	media.Stage, media.Attempts = "native_upload_attempting", 2
	job := bridgeMediaJob{ID: media.ID, Direction: "in", Phase: "native_upload_attempting"}
	e.mediaBusy = &job
	rejection := &chatgpt.UploadRejectedError{HTTPStatus: 400, Reason: "payload-too-large", LimitValue: "100", LimitUnit: "MB"}
	if err := e.acceptMedia(bridgeMediaResult{Job: job, Err: rejection}); err != nil {
		t.Fatal(err)
	}
	if media.Stage != "rejected" || media.Failure == nil || media.Failure.Phase != job.Phase || media.Failure.HTTPStatus != 400 || media.Failure.LimitValue != "100" || media.Failure.LimitUnit != "MB" {
		t.Fatal("upload rejection lost its phase, status or reported provider limit")
	}
	if len(e.state.Outbound) != 2 || !strings.Contains(e.state.Outbound[1].Text, "dot 原生附件上传被拒绝（HTTP 400）") || !strings.Contains(e.state.Outbound[1].Text, "100 MB") || !strings.Contains(e.state.Outbound[1].Text, "尚未发送到 dot") || e.state.Outbound[0].Text != "old download failure" {
		t.Fatal("a stale download notice swallowed the later native upload rejection")
	}
	if _, err := os.Stat(media.File.Path); err != nil {
		t.Fatal("rejected original was lost")
	}
}

func TestBridgeUnknownUploadNoticeDoesNotRepeatUploadOrReuseRejection(t *testing.T) {
	e, n, _ := bridgeTestMediaEngine(t)
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	in := e.state.Inbound[0]
	media := in.Media[0]
	media.File, _ = bridgeMediaFixture(e.mediaDir, "original.zip", "application/zip", 32, weixin.FileItemType)
	media.Stage = "native_upload_unknown"
	media.Failure = &bridgeMediaFailure{Phase: "native_upload_attempting", HTTPStatus: 500, Reason: "upload-outcome-unknown"}
	if err := e.mediaNotice(media.ID+":native_upload_attempting", "previous definite HTTP 400 rejection"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ready, err := e.ensureIncomingMedia(context.Background(), in)
		if err != nil || ready {
			t.Fatal("unknown upload was sent or failed to retain its notice")
		}
	}
	if n.uploads != 0 || len(n.sends) != 0 || media.Stage != "native_upload_unknown" || len(e.state.Outbound) != 2 || e.state.Outbound[0].Text != "previous definite HTTP 400 rejection" {
		t.Fatal("unknown recovery repeated upload, lost rejection evidence or duplicated notices")
	}
	notice := e.state.Outbound[1].Text
	if !strings.Contains(notice, "上传结果未知（HTTP 500）") || !strings.Contains(notice, "不会重复上传") || strings.Contains(notice, "上限") {
		t.Fatal("unknown upload was described as a definite size rejection")
	}
}

func TestBridgeMediaWorkerDoesNotBlockLaterText(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	w.downloadSize = 64
	w.downloading = make(chan struct{})
	w.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType), bridgeTestText("later", "peer", "later text", "context")}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverNative(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.downloading:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	if n.sentText != "later text" || e.state.Inbound[1].Status != "acknowledged" {
		t.Fatal("pending media blocked unrelated text delivery")
	}
	if err := e.queue("ordinary-reply", "ordinary reply", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 1 || w.sends[0].Text != "ordinary reply" {
		t.Fatal("pending media blocked native text replies")
	}
	close(w.release)
	bridgeFinishMedia(t, e)
}

func TestBridgeNativeUploadUnknownSurvivesRestartWithoutNewFile(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	w.downloadSize = 64
	n.uploadErr = &chatgpt.UploadOutcomeUnknownError{Cause: &chatgpt.HTTPError{Status: 500}}
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 2; phase++ {
		if err := e.deliverNative(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	e.state = bridgeClone(t, e.state)
	e.mediaBusy = nil
	n.uploadErr = nil
	for i := 0; i < 2; i++ {
		if err := e.deliverNative(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n.uploads != 1 || len(n.sends) != 0 || e.state.Inbound[0].Media[0].Stage != "native_upload_unknown" {
		t.Fatal("unknown native upload caused a duplicate file or false delivery")
	}
}

func TestBridgeNativeUploadTimeoutResponseIsNotRetried(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	w.downloadSize = 64
	n.uploadErr = &chatgpt.HTTPError{Status: 408}
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 2; phase++ {
		if err := e.deliverNative(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	n.uploadErr = nil
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	media := e.state.Inbound[0].Media[0]
	if n.uploads != 1 || media.Stage != "native_upload_unknown" || media.Cleaned {
		t.Fatal("HTTP408 was replayed or the original was removed")
	}
	if _, err := os.Stat(media.File.Path); err != nil {
		t.Fatal("unknown upload lost original")
	}
}

func TestBridgeSameNativeFileIsNotRepeatedForTextEdit(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	n.downloadSize = 16
	m := bridgeNativeWithFile("file-id", "")
	if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 4; phase++ {
		if err := e.deliverWeixin(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Content.Text = "edited caption"
	m.Updated = time.Now().UTC()
	if err := e.queueNative(m, true, bridgeHash(m)); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.mediaSends) != 1 || w.uploads != 1 || len(n.downloads) != 1 || len(w.sends) != 2 || w.sends[1].Text != "dot 更新：\nedited caption" {
		t.Fatal("a text edit repeated unchanged attachment bytes")
	}
}

func TestBridgeUnknownMediaMessageAllowsLaterTextAndKeepsFiles(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	n.downloadSize = 16
	m := bridgeNativeWithFile("file-id", "")
	if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 4; phase++ {
		if err := e.deliverWeixin(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	w.mediaSendErr = weixin.ErrOutcomeUnknown
	if err := e.queue("later-text", "later native text", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	mediaOut := e.state.Outbound[1]
	if mediaOut.Status != "unknown" || mediaOut.Media.Cleaned || len(w.sends) != 2 || w.sends[1].Text != "later native text" {
		t.Fatal("unknown media blocked unrelated text or cleaned the original")
	}
	for _, path := range []string{mediaOut.Media.File.Path, mediaOut.Media.Plan.CipherPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("unknown media lost staged byte evidence")
		}
	}
}

func TestBridgeUploadedMediaKnownRejectionResumesSamePayloadAfterRestart(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	n.downloadSize, n.downloadMIME = 32, "image/png"
	m := bridgeNativeWithFile("image-first", "")
	m.Content.Text = ""
	for _, fileID := range []string{"image-second", "image-third"} {
		raw, _ := json.Marshal(chatgpt.FileReference{Type: "file", FileID: fileID})
		m.Content.Attachments = append(m.Content.Attachments, raw)
	}
	if err := e.queueNative(m, false, bridgeNativeFingerprint(m)); err != nil {
		t.Fatal(err)
	}
	for phase := 0; phase < 4; phase++ {
		if err := e.deliverWeixin(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridgeFinishMedia(t, e)
	}
	w.mediaSendErr = &weixin.APIError{Op: "sendmessage", Kind: weixin.ErrAPI, Ret: -2}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := e.state.Outbound[0]
	key, clientID, text, mediaHash := first.Key, first.ClientID, first.Text, bridgeHash(first.Media)
	failure := *first.LastFailure
	for pass := 0; pass < 2; pass++ {
		if err := e.deliverWeixin(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.mediaSends) != 1 || len(n.downloads) != 1 || w.slots != 1 || w.uploads != 1 ||
		first.Status != "rejected" || first.Attempts != 1 || e.state.Outbound[1].Media.Stage != "received" || e.state.Outbound[2].Media.Stage != "received" {
		t.Fatal("same-context rejection replayed a mutation or later images overtook the blocked original")
	}
	e.state = bridgeClone(t, e.state) // Resume with exactly the same journal identities.
	e.state.ContextToken = "renewed-context"
	var durable *bridgeState
	e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
	w.beforeMediaSend = func(input weixin.SendMediaRequest) {
		if durable == nil {
			t.Fatal("media POST lacked a durable journal")
		}
		for _, saved := range durable.Outbound {
			if saved.ClientID != input.ClientID {
				continue
			}
			if saved.Status != "attempting" || saved.ContextToken != input.ContextToken ||
				bridgeHash(saved.Media.Uploaded) != bridgeHash(input.Uploaded) {
				t.Fatal("media POST changed its uploaded payload or preceded the durable attempt")
			}
			if input.ClientID == clientID && (saved.Key != key || saved.Text != text || bridgeHash(saved.Media) != mediaHash || *saved.LastFailure != failure) {
				t.Fatal("renewed-context retry changed the original media identity or prior failure evidence")
			}
			return
		}
		t.Fatal("media POST created an unjournalled identity")
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	first = e.state.Outbound[0]
	if len(w.mediaSends) != 2 || first.Status != "rejected" || first.Attempts != 2 || first.ContextToken != "renewed-context" ||
		len(n.downloads) != 1 || w.slots != 1 || w.uploads != 1 {
		t.Fatal("renewed context did not permit exactly one final-message retry, or repeated the media upload")
	}
	e.state = bridgeClone(t, e.state)
	e.state.ContextToken, w.mediaSendErr = "next-context", nil
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The first ACK releases the remaining images in the same native message.
	for phase := 0; phase < 8; phase++ {
		if e.mediaBusy == nil {
			t.Fatal("remaining image did not start its next media phase")
		}
		bridgeFinishMedia(t, e)
		if err := e.deliverWeixin(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.state.Outbound) != 3 || len(w.mediaSends) != 5 || len(n.downloads) != 3 || w.slots != 3 || w.uploads != 3 {
		t.Fatal("multi-image recovery duplicated media uploads or dropped a later image")
	}
	for _, out := range e.state.Outbound {
		if out.Status != "acknowledged" || out.WeixinID == "" || !out.Media.Cleaned {
			t.Fatal("a retained image did not reach a reliable ACK and cleanup")
		}
	}
	if e.state.Outbound[0].Attempts != 3 || e.state.Outbound[1].Attempts != 1 || e.state.Outbound[2].Attempts != 1 {
		t.Fatal("final-message attempt counts lost the original rejection evidence")
	}
	m.Content.Text = "later edited caption"
	if err := e.queueNative(m, true, bridgeNativeFingerprint(m)); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 4 || len(w.mediaSends) != 5 || len(n.downloads) != 3 || w.slots != 3 || w.uploads != 3 {
		t.Fatal("a caption edit resent acknowledged image originals")
	}
}

func TestBridgeUploadedMediaRenewalRetainsUnknownAndInvalidRetries(t *testing.T) {
	for _, test := range []string{"unknown-error", "mismatched-ack", "other-ret", "http-status", "errcode", "url-unknown", "cleaned", "wrong-kind", "invalid-uploaded"} {
		t.Run(test, func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			n.downloadSize, n.downloadMIME = 32, "image/png"
			m := bridgeNativeWithFile("image-original", "")
			m.Content.Text = ""
			if err := e.queueNative(m, false, bridgeNativeFingerprint(m)); err != nil {
				t.Fatal(err)
			}
			for phase := 0; phase < 4; phase++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			out := e.state.Outbound[0]
			if test == "unknown-error" || test == "mismatched-ack" {
				if test == "unknown-error" {
					w.mediaSendErr = weixin.ErrOutcomeUnknown
				} else {
					e.weixin = &bridgeDispatchMismatchedMediaACK{w}
				}
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				e.state = bridgeClone(t, e.state)
				e.state.ContextToken, w.mediaSendErr = "renewed-context", nil
				before := bridgeHash(e.state.Outbound[0])
				for pass := 0; pass < 2; pass++ {
					if err := e.deliverWeixin(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if bridgeHash(e.state.Outbound[0]) != before || len(w.mediaSends) != 1 ||
					w.slots != 1 || w.uploads != 1 || len(n.downloads) != 1 {
					t.Fatal("fresh context replayed or changed an unknown media result")
				}
				return
			}
			out.Status, out.ContextToken, out.Attempts = "rejected", "old-context", 1
			out.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: -2}
			switch test {
			case "other-ret":
				out.LastFailure.Ret = -3
			case "http-status":
				out.LastFailure.HTTPStatus = 413
			case "errcode":
				out.LastFailure.ErrCode = 4
			case "url-unknown":
				out.Media.Stage = "url_unknown"
			case "cleaned":
				out.Media.Cleaned = true
			case "wrong-kind":
				out.DeliveryKind = ""
			case "invalid-uploaded":
				out.Media.Uploaded.DownloadParam = ""
			}
			before := bridgeHash(out)
			if bridgeResumeContextRejection(out, "renewed-context") || bridgeHash(out) != before {
				t.Fatal("media renewal admitted another rejection or an invalid/unconfirmed original")
			}
		})
	}
}

func TestBridgeShutdownRemovesUncommittedDownloadOrCipher(t *testing.T) {
	for _, phase := range []string{"downloading", "preparing"} {
		t.Run(phase, func(t *testing.T) {
			e, _, _ := bridgeTestMediaEngine(t)
			file, err := bridgeMediaFixture(e.mediaDir, "original.bin", "application/octet-stream", 16, weixin.FileItemType)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			e.startMedia(ctx, bridgeMediaJob{ID: "pending", Direction: "out", Phase: phase}, func() bridgeMediaResult {
				<-ctx.Done()
				if phase == "downloading" {
					return bridgeMediaResult{File: file}
				}
				return bridgeMediaResult{Plan: &weixin.MediaUploadPlan{CipherPath: file.Path}}
			})
			cancel()
			e.shutdownMedia()
			if _, err := os.Stat(file.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancelled worker stranded an uncommitted temporary file")
			}
		})
	}
}

func bridgeNativeWithFile(id, mime string) chatgpt.Message {
	m := chatgpt.Message{ID: "native-output", Sender: "dot-member"}
	m.Content.Text = "result caption"
	raw, _ := json.Marshal(chatgpt.FileReference{Type: "file", FileID: id})
	m.Content.Attachments = []json.RawMessage{raw}
	return m
}

func TestBridgeNativeMediaStagesPersistBeforeNetworkAndSend(t *testing.T) {
	for _, mime := range []string{"application/octet-stream", "image/png", "video/mp4"} {
		t.Run(mime, func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			n.downloadSize = 73
			n.downloadMIME = mime
			m := bridgeNativeWithFile("native-file", mime)
			if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
				t.Fatal(err)
			}
			var durable *bridgeState
			e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
			w.beforeSlot = func(plan *weixin.MediaUploadPlan) error {
				saved := durable.Outbound[1].Media
				if saved.Stage != "url_attempting" || saved.Plan.FileKey != plan.FileKey {
					return errors.New("slot requested without durable plan")
				}
				return nil
			}
			w.beforeUpload = func(plan *weixin.MediaUploadPlan) error {
				saved := durable.Outbound[1].Media
				if saved.Stage != "upload_attempting" || saved.Plan.UploadURL != plan.UploadURL {
					return errors.New("CDN POST without durable slot")
				}
				return nil
			}
			for phase := 0; phase < 4; phase++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(w.sends) != 1 || len(w.mediaSends) != 1 || w.slots != 1 || w.uploads != 1 || len(n.downloads) != 1 || n.downloads[0] != "native-file" {
				t.Fatal("native attachment did not follow one staged Weixin upload/send")
			}
			out := e.state.Outbound[1]
			if out.Status != "acknowledged" || out.WeixinID == "" || out.Media.Stage != "uploaded" || out.Media.File.Size != 73 {
				t.Fatal("media ACK or byte identity is missing")
			}
			if !out.Media.Cleaned {
				t.Fatal("confirmed output did not clean temporary files")
			}
			for _, path := range []string{out.Media.File.Path, out.Media.Plan.CipherPath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("confirmed original/cipher snapshot remained")
				}
			}
		})
	}
}

func TestBridgeWeixinMediaUnknownAtEveryMutationDoesNotRepeat(t *testing.T) {
	for _, phase := range []string{"url", "upload", "send"} {
		t.Run(phase, func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			n.downloadSize = 32
			m := bridgeNativeWithFile("native-file", "")
			if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "url":
				w.slotErr = weixin.ErrOutcomeUnknown
			case "upload":
				w.uploadErr = weixin.ErrOutcomeUnknown
			case "send":
				w.mediaSendErr = weixin.ErrOutcomeUnknown
			}
			steps := 4
			if phase == "url" {
				steps = 3
			}
			for i := 0; i < steps; i++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			beforeSlots, beforeUploads, beforeSends := w.slots, w.uploads, len(w.mediaSends)
			e.state = bridgeClone(t, e.state)
			e.mediaBusy = nil
			e.state.ContextToken = "renewed-context"
			w.slotErr = nil
			w.uploadErr = nil
			w.mediaSendErr = nil
			for i := 0; i < 2; i++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if w.slots != beforeSlots || w.uploads != beforeUploads || len(w.mediaSends) != beforeSends {
				t.Fatal("unknown media mutation was repeated after recovery")
			}
			if phase == "send" && e.state.Outbound[1].Status != "unknown" {
				t.Fatal("unknown final media send was declared successful")
			}
		})
	}
}

func TestBridgeThreadCardIsNotDownloadedAsFile(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	m := chatgpt.Message{ID: "task-card", Sender: "dot-member"}
	m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"thread","thread_id":"task-id"}`)}
	if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.downloads) != 0 || len(w.sends) != 1 || !strings.Contains(w.sends[0].Text, "https://chatgpt.com/") || !strings.Contains(w.sends[0].Text, "不会替代原生审批") {
		t.Fatal("task card was downloaded or falsely represented as text approval")
	}
}

func TestBridgeV1MigrationKeepsHistoryIdentityAndUnsupportedUnplayed(t *testing.T) {
	e, n, _ := bridgeTestMediaEngine(t)
	m := bridgeTestMediaMessage(weixin.FileItemType)
	raw, _ := json.Marshal(m)
	e.state.Inbound = []*bridgeInbound{{Key: bridgeInboundKey(m), Message: m, Raw: raw, Status: "unsupported"}}
	e.state.Account = "account"
	e.state.User = "user"
	e.state.Started = time.Now().UTC()
	before := bridgeClone(t, e.state)
	if !bridgeMigrateState(e.state) || e.state.Version != 2 {
		t.Fatal("v1 journal was not migrated")
	}
	before.Version = 2
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(e.state)
	if string(a) != string(b) {
		t.Fatal("migration changed identity, history or queue state")
	}
	c := &chatgpt.Client{Identity: chatgpt.Identity{Account: "account", User: "user"}}
	if !bridgeStateValid(e.state, c, chatgpt.Profile{ID: "dot", Room: "room"}, "dot-member", weixin.Credentials{BotID: "bot", UserID: "peer"}) {
		t.Fatal("migrated journal was rejected")
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 0 || e.mediaBusy != nil {
		t.Fatal("migration replayed already-reported unsupported media")
	}
}

func TestBridgeNativeRejectionKeepsOriginalAndReportsProvider(t *testing.T) {
	for _, status := range []int{413, 415} {
		t.Run(fmt.Sprintf("HTTP-%d", status), func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			w.downloadSize = 45
			n.uploadErr = &chatgpt.HTTPError{Status: status}
			if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
				t.Fatal(err)
			}
			for phase := 0; phase < 2; phase++ {
				if err := e.deliverNative(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			media := e.state.Inbound[0].Media[0]
			before := bridgeHash(media)
			for pass := 0; pass < 2; pass++ {
				if err := e.deliverNative(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if media.Stage != "rejected" || media.NativeFileID != "" || n.uploads != 1 || len(n.sends) != 0 ||
				media.Failure == nil || media.Failure.HTTPStatus != status || media.Failure.Phase != "native_upload_attempting" ||
				len(e.state.Outbound) != 1 || !strings.Contains(e.state.Outbound[0].Text, fmt.Sprintf("HTTP %d", status)) ||
				!strings.Contains(e.state.Outbound[0].Text, "dot") || bridgeHash(media) != before {
				t.Fatal("native rejection repeated an upload, produced false success, or lost the provider failure")
			}
			if err := bridgeCheckLocalFile(context.Background(), media.File, e.mediaDir); err != nil {
				t.Fatal("rejected original was removed or changed")
			}
		})
	}
}

func TestBridgeWeixinRejectionKeepsOriginalWithoutRetryOrSuccess(t *testing.T) {
	for _, phase := range []string{"url", "upload", "send"} {
		for _, status := range []int{413, 415} {
			t.Run(fmt.Sprintf("%s/HTTP-%d", phase, status), func(t *testing.T) {
				e, n, w := bridgeTestMediaEngine(t)
				n.downloadSize = 32
				m := bridgeNativeWithFile("native-file", "")
				if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
					t.Fatal(err)
				}
				rejection := &weixin.APIError{Kind: weixin.ErrAPI, HTTPStatus: status}
				steps, wantPhase := 4, "sendmessage"
				switch phase {
				case "url":
					w.slotErr, steps, wantPhase = rejection, 3, "url_attempting"
				case "upload":
					w.uploadErr, wantPhase = rejection, "upload_attempting"
				case "send":
					w.mediaSendErr = rejection
				}
				for step := 0; step < steps; step++ {
					if err := e.deliverWeixin(context.Background()); err != nil {
						t.Fatal(err)
					}
					bridgeFinishMedia(t, e)
				}
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				out := e.state.Outbound[1]
				media := out.Media
				if media.Stage != "rejected" || media.Cleaned || out.WeixinID != "" || out.Status == "acknowledged" ||
					media.Failure == nil || media.Failure.HTTPStatus != status || media.Failure.Phase != wantPhase {
					t.Fatal("platform rejection lost its identity, removed the original, or declared media delivered")
				}
				if phase == "send" && (out.Status != "rejected" || out.LastFailure == nil || out.LastFailure.HTTPStatus != status) {
					t.Fatal("final message rejection lost its send receipt")
				}
				if len(e.state.Outbound) != 3 || !strings.Contains(e.state.Outbound[2].Text, "微信") ||
					!strings.Contains(e.state.Outbound[2].Text, fmt.Sprintf("HTTP %d", status)) {
					t.Fatal("platform rejection was not reported accurately")
				}
				if err := bridgeCheckLocalFile(context.Background(), media.File, e.mediaDir); err != nil {
					t.Fatal("rejected original was removed or changed")
				}
				if _, err := os.Stat(media.Plan.CipherPath); err != nil {
					t.Fatal("rejected cipher snapshot was removed")
				}
				beforeSlots, beforeUploads, beforeSends := w.slots, w.uploads, len(w.mediaSends)
				e.state = bridgeClone(t, e.state) // Recover from the persisted rejection.
				w.slotErr, w.uploadErr, w.mediaSendErr = nil, nil, nil
				for pass := 0; pass < 2; pass++ {
					if err := e.deliverWeixin(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if w.slots != beforeSlots || w.uploads != beforeUploads || len(w.mediaSends) != beforeSends || e.mediaBusy != nil {
					t.Fatal("retained rejection replayed a mutation after restart")
				}
			})
		}
	}
}

func TestBridgeMutatedPrivateOriginalCannotBeUploaded(t *testing.T) {
	e, n, w := bridgeTestMediaEngine(t)
	w.downloadSize = 45
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestMediaMessage(weixin.FileItemType)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridgeFinishMedia(t, e)
	file := e.state.Inbound[0].Media[0].File
	if err := os.WriteFile(file.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridgeFinishMedia(t, e)
	if n.uploads != 0 || len(n.sends) != 0 || e.state.Inbound[0].Media[0].Stage != "rejected" {
		t.Fatal("modified original was uploaded or declared delivered")
	}
	if filepath.Dir(file.Path) != e.mediaDir {
		t.Fatal("unexpected fixture path")
	}
}

func TestBridgeNativeDownloadRetriesOnlyTemporaryReads(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   *chatgpt.AttachmentReadError
		retry bool
	}{
		{"not-ready", &chatgpt.AttachmentReadError{Retryable: true}, true},
		{"storage-unavailable", &chatgpt.AttachmentReadError{HTTPStatus: 503, Retryable: true}, true},
		{"storage-denied", &chatgpt.AttachmentReadError{HTTPStatus: 403}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			n.downloadErr = test.err
			m := bridgeNativeWithFile("native-file", "")
			if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
				t.Fatal(err)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			bridgeFinishMedia(t, e)
			media := e.state.Outbound[1].Media
			want := "rejected"
			if test.retry {
				want = "received"
			}
			if media.Stage != want || w.slots != 0 || w.uploads != 0 || len(w.mediaSends) != 0 {
				t.Fatal("download failure retried a mutation or used the wrong state")
			}
			if !strings.Contains(e.state.Outbound[2].Text, "dot 原生下载") {
				t.Fatal("native download failure was attributed to Weixin")
			}
			n.downloadErr = nil
			media.RetryAt = time.Time{}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !test.retry {
				if len(n.downloads) != 1 || e.mediaBusy != nil {
					t.Fatal("permanently rejected native download was repeated")
				}
				return
			}
			bridgeFinishMedia(t, e)
			if len(n.downloads) != 2 || media.Stage != "downloaded" {
				t.Fatal("temporary native GET could not resume safely")
			}
		})
	}
}

func bridgeNativeFileWithCapability(url, status string) chatgpt.Message {
	m := bridgeNativeWithFile("native-file", "")
	raw, _ := json.Marshal(map[string]any{
		"attachment_id": "attachment-id", "type": "file", "file_id": "native-file",
		"file": map[string]any{
			"id": "native-file", "file_id": "native-file", "status": status,
			"permission_set": "private", "download_url": url,
			"mime_type": "text/plain", "image_metadata": nil, "name": "probe.txt",
		},
	})
	m.Content.Attachments = []json.RawMessage{raw}
	return m
}

func TestBridgeNativeDownloadCapabilityRefreshIsNotAnEdit(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-hash-%v", legacy), func(t *testing.T) {
			e, n, _ := bridgeTestMediaEngine(t)
			original := bridgeNativeFileWithCapability("https://example.invalid/first-capability", "ready")
			fingerprint := bridgeNativeFingerprint(original)
			if legacy {
				fingerprint = bridgeHash(original)
			}
			if err := e.queueNative(original, false, fingerprint); err != nil {
				t.Fatal(err)
			}
			e.state.NativeSeen[original.ID] = fingerprint
			// The caption has already been sent; retain its original saved Native
			// snapshot exactly as the existing private journal does.
			e.state.Outbound[0].Status = "acknowledged"
			e.state.Outbound[0].WeixinID = "caption-ack"
			before := bridgeClone(t, e.state)
			for _, url := range []string{"https://example.invalid/refreshed-capability", "https://example.invalid/next-capability"} {
				refreshed := bridgeNativeFileWithCapability(url, "ready")
				n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{refreshed}}}
				if err := e.syncNative(context.Background(), true); err != nil {
					t.Fatal(err)
				}
				if len(e.state.Outbound) != len(before.Outbound) || e.state.NativeSeen[original.ID] != bridgeNativeFingerprint(original) {
					t.Fatal("temporary file URL refresh queued a false edit or failed to upgrade the hash")
				}
				for i := range before.Outbound {
					if bridgeHash(before.Outbound[i]) != bridgeHash(e.state.Outbound[i]) {
						t.Fatal("URL normalization rewrote an existing delivery or its original snapshot")
					}
				}
			}
			edited := bridgeNativeFileWithCapability("https://example.invalid/edited-capability", "ready")
			edited.Content.Text = "actually edited result caption"
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{edited}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != len(before.Outbound)+1 || e.state.Outbound[len(e.state.Outbound)-1].Text != "dot 更新：\nactually edited result caption" {
				t.Fatal("a real text edit was suppressed or the original file was queued again")
			}
		})
	}
}

func TestBridgeNativeFingerprintKeepsRealAttachmentChanges(t *testing.T) {
	original := bridgeNativeFileWithCapability("https://example.invalid/capability", "ready")
	for _, status := range []string{"processing", "failed"} {
		changed := bridgeNativeFileWithCapability("https://example.invalid/refreshed", status)
		if bridgeNativeFingerprint(original) == bridgeNativeFingerprint(changed) {
			t.Fatal("native attachment status change was hidden")
		}
	}
	var attachment map[string]json.RawMessage
	if err := json.Unmarshal(original.Content.Attachments[0], &attachment); err != nil {
		t.Fatal(err)
	}
	attachment["file_id"] = json.RawMessage(`"another-native-file"`)
	changed := original
	raw, _ := json.Marshal(attachment)
	changed.Content.Attachments = []json.RawMessage{raw}
	if bridgeNativeFingerprint(original) == bridgeNativeFingerprint(changed) {
		t.Fatal("native attachment identity change was hidden")
	}
}

func TestBridgeRetiresOnlyConfirmedMetadataUpdatesAndUnblocksText(t *testing.T) {
	e, _, w := bridgeTestMediaEngine(t)
	queue := func(m chatgpt.Message, edited bool) *bridgeOutbound {
		t.Helper()
		text := m.Content.Text
		if edited {
			text = "dot 更新：\n" + text
		}
		if err := e.queue("native:"+m.ID+":"+bridgeHash(m), text, &m); err != nil {
			t.Fatal(err)
		}
		return e.state.Outbound[len(e.state.Outbound)-1]
	}
	original := bridgeNativeFileWithCapability("https://example.invalid/original", "ready")
	ack := queue(original, false)
	ack.Status, ack.ContextToken, ack.WeixinID, ack.Attempts = "acknowledged", "context", "original-caption-ack", 1
	rejected := queue(bridgeNativeFileWithCapability("https://example.invalid/rejected-refresh", "ready"), true)
	rejected.Status, rejected.ContextToken, rejected.Attempts = "rejected", "context", 1
	rejected.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: 123, ErrCode: 456}
	ready := queue(bridgeNativeFileWithCapability("https://example.invalid/ready-refresh", "ready"), true)
	edited := bridgeNativeFileWithCapability("https://example.invalid/actual-edit", "ready")
	edited.Content.Text = "genuine new caption"
	trueEdit := queue(edited, true)
	unknown := queue(bridgeNativeFileWithCapability("https://example.invalid/unknown-refresh", "ready"), true)
	unknown.Status, unknown.ContextToken, unknown.Attempts = "unknown", "context", 1
	before := bridgeClone(t, e.state)
	if err := e.retireMetadataOnlyUpdates(); err != nil {
		t.Fatal(err)
	}
	for _, out := range []*bridgeOutbound{rejected, ready} {
		if out.Status != "superseded_metadata_only" || out.MetadataSupersession == nil || out.MetadataSupersession.AcknowledgedKey != ack.Key || !bridgeOutboundValid(out, e.state) {
			t.Fatal("confirmed metadata-only revision was not durably retired")
		}
	}
	if rejected.MetadataSupersession.PreviousStatus != "rejected" || ready.MetadataSupersession.PreviousStatus != "ready" || trueEdit.Status != "ready" || unknown.Status != "unknown" {
		t.Fatal("recovery changed an actual edit or an uncertain delivery")
	}
	expected := bridgeClone(t, before)
	expected.Outbound[1].Status, expected.Outbound[1].MetadataSupersession = rejected.Status, rejected.MetadataSupersession
	expected.Outbound[2].Status, expected.Outbound[2].MetadataSupersession = ready.Status, ready.MetadataSupersession
	if bridgeHash(e.state) != bridgeHash(expected) {
		t.Fatal("recovery rewrote attempts, payload, old receipt or other journal facts")
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 1 || w.sends[0].Text != "dot 更新：\ngenuine new caption" || unknown.Status != "unknown" {
		t.Fatal("metadata rejection still blocked true text, or an unknown message was repeated")
	}
	rejected.MetadataSupersession.AcknowledgedKey = trueEdit.Key
	if bridgeOutboundValid(rejected, e.state) {
		t.Fatal("a mismatched receipt validated metadata retirement")
	}
}

func TestBridgeSendFailureJournalContainsOnlySafeProtocolFacts(t *testing.T) {
	e, _, w := bridgeTestEngine()
	e.state.ContextToken = "context"
	if err := e.queue("failure", "ordinary text", nil); err != nil {
		t.Fatal(err)
	}
	w.err = &weixin.APIError{Op: "https://example.invalid/secret-operation", Kind: errors.New("secret-error-text"), HTTPStatus: 400, Ret: 123, ErrCode: 456}
	if err := e.deliverWeixin(context.Background()); err == nil {
		t.Fatal("explicit invalid request did not stop delivery")
	}
	record := e.state.Outbound[0].LastFailure
	if record == nil || record.Op != "sendmessage" || record.HTTPStatus != 400 || record.Ret != 123 || record.ErrCode != 456 {
		t.Fatal("send rejection lost safe protocol facts")
	}
	encoded, _ := json.Marshal(e.state)
	if strings.Contains(string(encoded), "secret-error-text") || strings.Contains(string(encoded), "secret-operation") {
		t.Fatal("raw error or untrusted operation entered the private journal")
	}
}
