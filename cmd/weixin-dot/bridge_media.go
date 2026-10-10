// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

// These records contain private file paths, CDN capabilities and encryption
// keys. They belong only in the private journal, never ordinary log output.
type bridgeIncomingMedia struct {
	ID           string                  `json:"id"`
	Source       weixin.MessageItem      `json:"source"`
	Stage        string                  `json:"stage"`
	File         *weixin.MediaFile       `json:"file,omitempty"`
	NativeFileID string                  `json:"native_file_id,omitempty"`
	Attempts     int                     `json:"attempts,omitempty"`
	RetryAt      time.Time               `json:"retry_at,omitempty"`
	Cleaned      bool                    `json:"cleaned,omitempty"`
	ReadFailure  *bridgeMediaReadFailure `json:"read_failure,omitempty"`
	Failure      *bridgeMediaFailure     `json:"failure,omitempty"`
}

type bridgeMediaFailure struct {
	Phase      string `json:"phase"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Reason     string `json:"reason"`
	LimitValue string `json:"limit_value,omitempty"`
	LimitUnit  string `json:"limit_unit,omitempty"`
}

// These diagnostics retain only stable protocol facts, never CDN URLs or keys.
type bridgeMediaReadFailure struct {
	Reason        string `json:"reason"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	BytesRead     int64  `json:"bytes_read"`
	ExpectedBytes int64  `json:"expected_bytes"`
}

type bridgeOutgoingMedia struct {
	ID           string                  `json:"id"`
	NativeFileID string                  `json:"native_file_id"`
	Stage        string                  `json:"stage"`
	File         *weixin.MediaFile       `json:"file,omitempty"`
	Plan         *weixin.MediaUploadPlan `json:"upload_plan,omitempty"`
	Uploaded     *weixin.UploadedMedia   `json:"uploaded,omitempty"`
	Attempts     int                     `json:"attempts,omitempty"`
	RetryAt      time.Time               `json:"retry_at,omitempty"`
	Cleaned      bool                    `json:"cleaned,omitempty"`
	Failure      *bridgeMediaFailure     `json:"failure,omitempty"`
}

type bridgeNativeMedia interface {
	UploadFile(context.Context, string, string, string, string) (string, error)
	DownloadAttachmentTo(context.Context, string, string, io.Writer) (*chatgpt.FileInfo, error)
}

type bridgeWeixinMedia interface {
	DownloadMedia(context.Context, weixin.MessageItem, weixin.MediaOptions) (*weixin.MediaFile, error)
	RequestMediaUploadURL(context.Context, *weixin.MediaUploadPlan) error
	UploadPreparedMedia(context.Context, *weixin.MediaUploadPlan, weixin.MediaOptions) (*weixin.UploadedMedia, error)
	SendMedia(context.Context, weixin.SendMediaRequest) (*weixin.SendResult, error)
}

type bridgeMediaJob struct {
	ID, Direction, Phase string
}

type bridgeMediaResult struct {
	Job      bridgeMediaJob
	File     *weixin.MediaFile
	Plan     *weixin.MediaUploadPlan
	Uploaded *weixin.UploadedMedia
	FileID   string
	Err      error
}

func bridgePayload(m weixin.Message) (string, []weixin.MessageItem, bool) {
	var text []string
	var media []weixin.MessageItem
	for _, item := range m.ItemList {
		if item.RefMessage != nil || len(item.ToolCallStart)+len(item.ToolCallResult) != 0 {
			return "", nil, false
		}
		switch item.Type {
		case weixin.TextItemType:
			if item.TextItem == nil || len(item.FileItem)+len(item.ImageItem)+len(item.VideoItem)+len(item.VoiceItem) != 0 {
				return "", nil, false
			}
			text = append(text, item.TextItem.Text)
		case weixin.FileItemType, weixin.ImageItemType, weixin.VideoItemType:
			if item.TextItem != nil || len(item.VoiceItem) != 0 {
				return "", nil, false
			}
			media = append(media, item)
		case weixin.VoiceItemType:
			if len(item.VoiceItem) == 0 || item.TextItem != nil || len(item.FileItem)+len(item.ImageItem)+len(item.VideoItem) != 0 {
				return "", nil, false
			}
			var voice struct {
				Media map[string]json.RawMessage `json:"media"`
			}
			if json.Unmarshal(item.VoiceItem, &voice) != nil || len(voice.Media) == 0 {
				return "", nil, false
			}
			media = append(media, item)
		default:
			return "", nil, false
		}
	}
	joined := strings.Join(text, "\n")
	return joined, media, strings.TrimSpace(joined) != "" || len(media) != 0
}

func bridgeInitIncomingMedia(items []weixin.MessageItem) ([]*bridgeIncomingMedia, error) {
	var out []*bridgeIncomingMedia
	for _, item := range items {
		id, err := bridgeID()
		if err != nil {
			return nil, err
		}
		out = append(out, &bridgeIncomingMedia{ID: id, Source: item, Stage: "received"})
	}
	return out, nil
}

func bridgeInboundNativePayload(in *bridgeInbound) (string, []chatgpt.FileReference) {
	text, _, _ := bridgePayload(in.Message)
	var refs []chatgpt.FileReference
	for _, media := range in.Media {
		if media.Stage == "native_uploaded" {
			refs = append(refs, chatgpt.FileReference{Type: "file", FileID: media.NativeFileID})
		}
	}
	return text, refs
}

func bridgeNativeFilesMatch(m *chatgpt.Message, refs []chatgpt.FileReference) bool {
	var actual, expected []string
	for _, raw := range m.Content.Attachments {
		var ref chatgpt.FileReference
		if json.Unmarshal(raw, &ref) != nil || ref.Type != "file" || ref.FileID == "" {
			return false
		}
		actual = append(actual, ref.FileID)
	}
	for _, ref := range refs {
		expected = append(expected, ref.FileID)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	return strings.Join(actual, "\x00") == strings.Join(expected, "\x00")
}

func (e *bridgeEngine) mediaOptions() weixin.MediaOptions {
	return weixin.MediaOptions{TempDir: e.mediaDir}
}

func (e *bridgeEngine) mediaRunning(id, phase string) bool {
	return e.mediaBusy != nil && e.mediaBusy.ID == id && e.mediaBusy.Phase == phase
}

func (e *bridgeEngine) startMedia(ctx context.Context, job bridgeMediaJob, work func() bridgeMediaResult) {
	if e.mediaResults == nil {
		e.mediaResults = make(chan bridgeMediaResult, 1)
	}
	e.mediaBusy = &job
	e.mediaWorkers.Add(1)
	go func() {
		defer e.mediaWorkers.Done()
		result := work()
		result.Job = job
		// One worker and one buffered result. Shutdown waits and drains this
		// result explicitly; cancellation cannot strand an uncommitted file.
		e.mediaResults <- result
	}()
}

func (e *bridgeEngine) ensureIncomingMedia(ctx context.Context, in *bridgeInbound) (bool, error) {
	n, nativeOK := e.native.(bridgeNativeMedia)
	w, weixinOK := e.weixin.(bridgeWeixinMedia)
	if !nativeOK || !weixinOK {
		return false, errors.New("media transport is unavailable; original input retained")
	}
	for _, media := range in.Media {
		if time.Now().Before(media.RetryAt) {
			return false, nil
		}
		switch media.Stage {
		case "native_uploaded":
			continue
		case "native_upload_unknown":
			status := 0
			if media.Failure != nil {
				status = media.Failure.HTTPStatus
			}
			unknown := &chatgpt.UploadOutcomeUnknownError{Cause: &chatgpt.HTTPError{Status: status}}
			before := len(e.state.Outbound)
			if err := e.mediaNotice(media.ID+":native_upload_attempting:unknown", bridgeMediaRejectionNotice("in", "native_upload_attempting", unknown)); err != nil {
				return false, err
			}
			if len(e.state.Outbound) != before {
				return false, e.store()
			}
			return false, nil
		case "rejected":
			return false, nil
		case "native_upload_attempting":
			if e.mediaRunning(media.ID, media.Stage) {
				return false, nil
			}
			media.Stage = strings.TrimSuffix(media.Stage, "attempting") + "unknown"
			if err := e.mediaNotice(media.ID, "附件上次上传的结果尚未确认，原件已保留；当前不会重复上传，也没有把这条附件消息交给 dot。"); err != nil {
				return false, err
			}
			return false, e.store()
		case "downloading":
			if e.mediaRunning(media.ID, "downloading") {
				return false, nil
			}
			media.Stage = "received" // GET may safely resume after a crash.
			if err := e.store(); err != nil {
				return false, err
			}
		}
		if e.mediaBusy != nil {
			return false, nil
		}
		media.Attempts++
		switch media.Stage {
		case "received":
			media.Stage = "downloading"
			if err := e.store(); err != nil {
				return false, err
			}
			source := media.Source
			opts := e.mediaOptions()
			e.startMedia(ctx, bridgeMediaJob{media.ID, "in", media.Stage}, func() bridgeMediaResult {
				file, err := w.DownloadMedia(ctx, source, opts)
				if err == nil {
					err = bridgeCheckLocalFile(ctx, file, opts.TempDir)
				}
				return bridgeMediaResult{File: file, Err: err}
			})
		case "downloaded":
			media.Stage = "native_upload_attempting"
			if err := e.store(); err != nil {
				return false, err
			}
			file := *media.File
			room := e.state.Room
			dir := e.mediaDir
			e.startMedia(ctx, bridgeMediaJob{media.ID, "in", media.Stage}, func() bridgeMediaResult {
				if err := bridgeCheckLocalFile(ctx, &file, dir); err != nil {
					return bridgeMediaResult{Err: err}
				}
				id, err := n.UploadFile(ctx, room, file.Name, file.MIME, file.Path)
				return bridgeMediaResult{FileID: id, Err: err}
			})
		default:
			return false, errors.New("incoming media journal stage is invalid")
		}
		return false, nil
	}
	return true, nil
}

func (e *bridgeEngine) ensureOutgoingMedia(ctx context.Context, out *bridgeOutbound) (bool, error) {
	media := out.Media
	if media == nil {
		return true, nil
	}
	n, nativeOK := e.native.(bridgeNativeMedia)
	w, weixinOK := e.weixin.(bridgeWeixinMedia)
	if !nativeOK || !weixinOK {
		return false, errors.New("media transport is unavailable; native file retained")
	}
	if time.Now().Before(media.RetryAt) {
		return false, nil
	}
	switch media.Stage {
	case "uploaded":
		return true, nil
	case "url_unknown", "upload_unknown", "rejected":
		return false, nil
	case "url_attempting", "upload_attempting":
		if e.mediaRunning(media.ID, media.Stage) {
			return false, nil
		}
		media.Stage = strings.TrimSuffix(media.Stage, "attempting") + "unknown"
		if err := e.mediaNotice(media.ID, "dot 附件上次回传上传的结果尚未确认，原件和上传记录已保留；当前不会重复上传或报告文件已送达。"); err != nil {
			return false, err
		}
		return false, e.store()
	case "downloading", "preparing":
		if e.mediaRunning(media.ID, media.Stage) {
			return false, nil
		}
		if media.Stage == "downloading" {
			media.Stage = "received"
		} else {
			media.Stage = "downloaded"
		}
		if err := e.store(); err != nil {
			return false, err
		}
	}
	if e.mediaBusy != nil {
		return false, nil
	}
	media.Attempts++
	switch media.Stage {
	case "received":
		media.Stage = "downloading"
		if err := e.store(); err != nil {
			return false, err
		}
		room, fileID, dir := e.state.Room, media.NativeFileID, e.mediaDir
		e.startMedia(ctx, bridgeMediaJob{media.ID, "out", media.Stage}, func() bridgeMediaResult {
			file, err := bridgeDownloadNative(ctx, n, room, fileID, dir)
			return bridgeMediaResult{File: file, Err: err}
		})
	case "downloaded":
		media.Stage = "preparing"
		if err := e.store(); err != nil {
			return false, err
		}
		file := *media.File
		opts := e.mediaOptions()
		peer := e.state.Peer
		prepare := e.prepareMedia
		if prepare == nil {
			prepare = weixin.PrepareMediaUpload
		}
		e.startMedia(ctx, bridgeMediaJob{media.ID, "out", media.Stage}, func() bridgeMediaResult {
			if err := bridgeCheckLocalFile(ctx, &file, opts.TempDir); err != nil {
				return bridgeMediaResult{Err: err}
			}
			plan, err := prepare(ctx, weixin.MediaUploadRequest{Path: file.Path, Name: file.Name, MIME: file.MIME, Type: file.Type, ToUserID: peer, SHA256: file.SHA256}, opts)
			return bridgeMediaResult{Plan: plan, Err: err}
		})
	case "prepared":
		media.Stage = "url_attempting"
		if err := e.store(); err != nil {
			return false, err
		}
		plan := *media.Plan
		e.startMedia(ctx, bridgeMediaJob{media.ID, "out", media.Stage}, func() bridgeMediaResult {
			err := w.RequestMediaUploadURL(ctx, &plan)
			return bridgeMediaResult{Plan: &plan, Err: err}
		})
	case "url_ready":
		media.Stage = "upload_attempting"
		if err := e.store(); err != nil {
			return false, err
		}
		plan := *media.Plan
		opts := e.mediaOptions()
		e.startMedia(ctx, bridgeMediaJob{media.ID, "out", media.Stage}, func() bridgeMediaResult {
			result, err := w.UploadPreparedMedia(ctx, &plan, opts)
			return bridgeMediaResult{Uploaded: result, Err: err}
		})
	default:
		return false, errors.New("outgoing media journal stage is invalid")
	}
	return false, nil
}

func (e *bridgeEngine) mediaNotice(id, text string) error {
	return e.queue("media-notice:"+id, text, nil)
}

func bridgeHTTPStatus(err error) int {
	var native *chatgpt.HTTPError
	if errors.As(err, &native) {
		return native.Status
	}
	var wx *weixin.APIError
	if errors.As(err, &wx) {
		return wx.HTTPStatus
	}
	var read *chatgpt.AttachmentReadError
	if errors.As(err, &read) {
		return read.HTTPStatus
	}
	var mediaRead *weixin.MediaReadError
	if errors.As(err, &mediaRead) {
		return mediaRead.HTTPStatus
	}
	return 0
}

func bridgeMediaRejectionNotice(direction, phase string, err error) string {
	if direction == "in" && phase == "native_upload_attempting" && bridgeMediaUnknown(err) {
		status := ""
		if code := bridgeHTTPStatus(err); code != 0 {
			status = fmt.Sprintf("（HTTP %d）", code)
		}
		return "dot 原生附件上传结果未知" + status + "。原件与记录已保留；尚未取得可靠的文件引用，当前不会重复上传或报告文件已送达。"
	}
	var uploadRejection *chatgpt.UploadRejectedError
	if direction == "in" && phase == "native_upload_attempting" && errors.As(err, &uploadRejection) {
		text := fmt.Sprintf("dot 原生附件上传被拒绝（HTTP %d）。", uploadRejection.HTTPStatus)
		switch uploadRejection.Reason {
		case "payload-too-large":
			text += "服务端报告文件过大。"
		case "unsupported-media-type":
			text += "服务端未接受这个附件类型。"
		}
		if uploadRejection.LimitValue != "" && uploadRejection.LimitUnit != "" {
			text += "服务端报告上限：" + uploadRejection.LimitValue + " " + uploadRejection.LimitUnit + "。"
		}
		return text + "原件已保留，尚未发送到 dot 对话。"
	}
	var mediaRead *weixin.MediaReadError
	if direction == "in" && phase == "downloading" && errors.As(err, &mediaRead) {
		reason := "原件下载未完成"
		switch mediaRead.Reason {
		case "transport", "read-interrupted", "no-progress":
			reason = "原件尚未收完，下载连接已中断"
		case "range-unsupported":
			reason = "下载连接中断，文件服务未接受从中断处续传"
		case "resume-identity-unavailable":
			reason = "下载连接中断，现有文件信息不足以核验续传原件"
		case "local-write", "local-seek":
			reason = "原件无法写入本机暂存文件"
		case "cancelled":
			reason = "原件下载已取消"
		case "http":
			reason = fmt.Sprintf("文件下载服务返回 HTTP %d", mediaRead.HTTPStatus)
		}
		text := "微信附件下载未完成：" + reason + "。"
		if mediaRead.ExpectedBytes >= 0 {
			text += fmt.Sprintf("已读取 %d / %d 字节。", mediaRead.BytesRead, mediaRead.ExpectedBytes)
		}
		if mediaRead.Retryable {
			text += "消息已保留，将继续尝试下载；尚未发送到 dot。"
		} else {
			text += "消息已保留，尚未发送到 dot。"
		}
		return text
	}
	side := "微信"
	if direction == "in" && phase == "native_upload_attempting" {
		side = "dot"
	}
	if direction == "out" && phase == "downloading" {
		side = "dot 原生下载"
	}
	status := bridgeHTTPStatus(err)
	detail := ""
	if status != 0 {
		detail = fmt.Sprintf("（HTTP %d）", status)
	}
	reason := ""
	if status == 413 {
		reason = "服务端报告文件过大。"
	} else if status == 415 {
		reason = "服务端未接受这个附件类型。"
	}
	return side + "附件通路拒绝或未能核验这次传输" + detail + "。" + reason + "消息与已取得的原件已保留，没有报告附件送达。"
}

func bridgeMediaUnknown(err error) bool {
	var unknown *chatgpt.UploadOutcomeUnknownError
	return errors.Is(err, weixin.ErrOutcomeUnknown) || errors.As(err, &unknown)
}

func bridgeMediaFailureRecord(phase string, err error) *bridgeMediaFailure {
	failure := &bridgeMediaFailure{Phase: phase, HTTPStatus: bridgeHTTPStatus(err), Reason: "unclassified"}
	switch failure.HTTPStatus {
	case 413:
		failure.Reason = "payload-too-large"
	case 415:
		failure.Reason = "unsupported-media-type"
	default:
		if failure.HTTPStatus != 0 {
			failure.Reason = "http-rejection"
		}
	}
	if bridgeMediaUnknown(err) {
		failure.Reason = "upload-outcome-unknown"
	}
	var rejection *chatgpt.UploadRejectedError
	if errors.As(err, &rejection) {
		failure.Reason, failure.LimitValue, failure.LimitUnit = rejection.Reason, rejection.LimitValue, rejection.LimitUnit
	}
	return failure
}

func bridgeMediaTemporary(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	status := bridgeHTTPStatus(err)
	if status == 429 || status == 408 || status >= 500 {
		return true
	}
	var wx *weixin.APIError
	if errors.As(err, &wx) && wx.HTTPStatus == 0 && wx.Ret == 0 && wx.ErrCode == 0 && errors.Is(err, weixin.ErrAPI) {
		return true
	}
	var mediaRead *weixin.MediaReadError
	if errors.As(err, &mediaRead) {
		return mediaRead.Retryable
	}
	var read *chatgpt.AttachmentReadError
	return errors.As(err, &read) && read.Retryable
}

func (e *bridgeEngine) acceptMedia(result bridgeMediaResult) error {
	if e.mediaBusy == nil || *e.mediaBusy != result.Job {
		return errors.New("media completion identity mismatch; delivery stopped")
	}
	e.mediaBusy = nil
	if result.Job.Direction == "in" {
		for _, in := range e.state.Inbound {
			for _, media := range in.Media {
				if media.ID != result.Job.ID {
					continue
				}
				if media.Stage != result.Job.Phase {
					return errors.New("incoming media completion stage mismatch")
				}
				if result.Err == nil {
					switch result.Job.Phase {
					case "downloading":
						if !bridgeFileRecordValid(result.File) {
							return errors.New("downloaded original identity validation failed")
						}
						media.File, media.Stage = result.File, "downloaded"
						media.ReadFailure = nil
					case "native_upload_attempting":
						if result.FileID == "" {
							media.Stage = "native_upload_unknown"
						} else {
							media.NativeFileID, media.Stage = result.FileID, "native_uploaded"
						}
					default:
						return errors.New("unexpected incoming media completion")
					}
				} else {
					media.Failure = bridgeMediaFailureRecord(result.Job.Phase, result.Err)
					var readFailure *weixin.MediaReadError
					if errors.As(result.Err, &readFailure) {
						media.ReadFailure = &bridgeMediaReadFailure{Reason: readFailure.Reason, HTTPStatus: readFailure.HTTPStatus, BytesRead: readFailure.BytesRead, ExpectedBytes: readFailure.ExpectedBytes}
						media.Failure.Reason = "read-" + readFailure.Reason
					}
					if result.File != nil {
						_ = os.Remove(result.File.Path)
					}
					switch {
					case result.Job.Phase == "native_upload_attempting" && (bridgeMediaUnknown(result.Err) || bridgeHTTPStatus(result.Err) == 408 || (bridgeHTTPStatus(result.Err) >= 300 && bridgeHTTPStatus(result.Err) < 400)):
						media.Stage = "native_upload_unknown"
					case result.Job.Phase == "native_upload_attempting" && (bridgeHTTPStatus(result.Err) == 413 || bridgeHTTPStatus(result.Err) == 415):
						media.Stage = "rejected"
					case result.Job.Phase == "native_upload_attempting" && (bridgeHTTPStatus(result.Err) == 401 || bridgeHTTPStatus(result.Err) == 403):
						media.Stage = "downloaded"
						media.RetryAt = time.Now().Add(bridgeRetryDelay(media.Attempts))
						if err := e.store(); err != nil {
							return err
						}
						return errors.New("dot media authentication rejected; reconnect required")
					case bridgeMediaTemporary(result.Err):
						if result.Job.Phase == "downloading" {
							media.Stage = "received"
						} else {
							media.Stage = "downloaded"
						}
						media.RetryAt = time.Now().Add(bridgeRetryDelay(media.Attempts))
					default:
						media.Stage = "rejected"
					}
					noticeID := media.ID + ":" + result.Job.Phase
					if result.Job.Phase == "native_upload_attempting" && bridgeMediaUnknown(result.Err) {
						noticeID += ":unknown"
					}
					if err := e.mediaNotice(noticeID, bridgeMediaRejectionNotice("in", result.Job.Phase, result.Err)); err != nil {
						return err
					}
				}
				e.event("incoming_media_" + media.Stage)
				return e.store()
			}
		}
	} else if result.Job.Direction == "out" {
		for _, out := range e.state.Outbound {
			media := out.Media
			if media == nil || media.ID != result.Job.ID {
				continue
			}
			if media.Stage != result.Job.Phase {
				return errors.New("outgoing media completion stage mismatch")
			}
			if result.Err == nil {
				switch result.Job.Phase {
				case "downloading":
					if !bridgeFileRecordValid(result.File) {
						return errors.New("downloaded native file identity validation failed")
					}
					media.File, media.Stage = result.File, "downloaded"
				case "preparing":
					if !bridgePlanValid(result.Plan, media.File, e.state.Peer, false) {
						return errors.New("prepared media identity validation failed")
					}
					media.Plan, media.Stage = result.Plan, "prepared"
				case "url_attempting":
					if !bridgePlanValid(result.Plan, media.File, e.state.Peer, true) {
						media.Stage = "url_unknown"
					} else {
						media.Plan, media.Stage = result.Plan, "url_ready"
					}
				case "upload_attempting":
					if !bridgeUploadedValid(result.Uploaded, media.Plan, e.state.Peer) {
						media.Stage = "upload_unknown"
					} else {
						media.Uploaded, media.Stage = result.Uploaded, "uploaded"
					}
				default:
					return errors.New("unexpected outgoing media completion")
				}
			} else {
				media.Failure = bridgeMediaFailureRecord(result.Job.Phase, result.Err)
				if result.File != nil {
					_ = os.Remove(result.File.Path)
				}
				switch {
				case bridgeMediaUnknown(result.Err):
					media.Stage = strings.TrimSuffix(result.Job.Phase, "attempting") + "unknown"
				case (result.Job.Phase == "url_attempting" || result.Job.Phase == "upload_attempting") && (bridgeHTTPStatus(result.Err) == 413 || bridgeHTTPStatus(result.Err) == 415):
					media.Stage = "rejected"
				case result.Job.Phase == "url_attempting" && bridgeWeixinFatal(result.Err):
					media.Stage = "prepared"
					media.RetryAt = time.Now().Add(bridgeRetryDelay(media.Attempts))
					if err := e.store(); err != nil {
						return err
					}
					return errors.New("Weixin media authorization rejected; reconnect required")
				case bridgeMediaTemporary(result.Err):
					switch result.Job.Phase {
					case "downloading":
						media.Stage = "received"
					case "preparing":
						media.Stage = "downloaded"
					case "url_attempting":
						media.Stage = "prepared"
					case "upload_attempting":
						media.Stage = "url_ready"
					}
					media.RetryAt = time.Now().Add(bridgeRetryDelay(media.Attempts))
				default:
					media.Stage = "rejected"
				}
				if err := e.mediaNotice(media.ID, bridgeMediaRejectionNotice("out", result.Job.Phase, result.Err)); err != nil {
					return err
				}
			}
			e.event("outgoing_media_" + media.Stage)
			return e.store()
		}
	}
	return errors.New("media completion has no matching journal entry; delivery stopped")
}

func bridgeFileRecordValid(file *weixin.MediaFile) bool {
	if file == nil || file.Path == "" || file.Name == "" || file.MIME == "" || file.Size < 0 {
		return false
	}
	if file.Type != weixin.FileItemType && file.Type != weixin.ImageItemType && file.Type != weixin.VideoItemType && file.Type != weixin.VoiceItemType {
		return false
	}
	digest, err := hex.DecodeString(file.SHA256)
	return err == nil && len(digest) == sha256.Size
}

func bridgeIncomingRecordsValid(records []*bridgeIncomingMedia) bool {
	seen := map[string]bool{}
	for _, media := range records {
		if media == nil || media.ID == "" || seen[media.ID] || media.Attempts < 0 {
			return false
		}
		seen[media.ID] = true
		switch media.Source.Type {
		case weixin.FileItemType, weixin.ImageItemType, weixin.VideoItemType, weixin.VoiceItemType:
		default:
			return false
		}
		if media.File != nil && !bridgeFileRecordValid(media.File) {
			return false
		}
		switch media.Stage {
		case "received", "downloading", "rejected":
		case "downloaded", "native_upload_attempting", "native_upload_unknown":
			if media.File == nil {
				return false
			}
		case "native_uploaded":
			if media.File == nil || media.NativeFileID == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func bridgeOutgoingRecordValid(media *bridgeOutgoingMedia, peer string) bool {
	if media == nil {
		return true
	}
	if media.ID == "" || media.NativeFileID == "" || media.Attempts < 0 {
		return false
	}
	if media.File != nil && !bridgeFileRecordValid(media.File) {
		return false
	}
	switch media.Stage {
	case "received", "downloading", "rejected":
		return true
	case "downloaded", "preparing":
		return media.File != nil
	case "prepared", "url_attempting", "url_unknown":
		return bridgePlanValid(media.Plan, media.File, peer, false)
	case "url_ready", "upload_attempting", "upload_unknown":
		return bridgePlanValid(media.Plan, media.File, peer, true)
	case "uploaded":
		return bridgePlanValid(media.Plan, media.File, peer, true) && bridgeUploadedValid(media.Uploaded, media.Plan, peer)
	default:
		return false
	}
}

func bridgePlanValid(plan *weixin.MediaUploadPlan, file *weixin.MediaFile, peer string, withURL bool) bool {
	if plan == nil || !bridgeFileRecordValid(file) || plan.FileKey == "" || plan.CipherPath == "" || plan.ToUserID != peer ||
		plan.RawSize != file.Size || plan.SHA256 != file.SHA256 || plan.Type != file.Type || plan.Name != file.Name || plan.MIME != file.MIME || plan.CipherSize <= 0 ||
		plan.CipherSize%16 != 0 || plan.CipherSize/16 != plan.RawSize/16+1 {
		return false
	}
	key, err := hex.DecodeString(plan.AESKeyHex)
	if err != nil || len(key) != 16 {
		return false
	}
	digest, err := hex.DecodeString(plan.CipherSHA256)
	if err != nil || len(digest) != sha256.Size {
		return false
	}
	md5, err := hex.DecodeString(plan.MD5)
	if err != nil || len(md5) != 16 {
		return false
	}
	if withURL {
		u, err := url.Parse(plan.UploadURL)
		return err == nil && u.Scheme == "https" && u.User == nil && u.Opaque == "" && u.Fragment == "" && (u.Port() == "" || u.Port() == "443") && (strings.ToLower(u.Hostname()) == "weixin.qq.com" || strings.HasSuffix(strings.ToLower(u.Hostname()), ".weixin.qq.com"))
	}
	return true
}

func bridgeUploadedValid(uploaded *weixin.UploadedMedia, plan *weixin.MediaUploadPlan, peer string) bool {
	return uploaded != nil && plan != nil && uploaded.FileKey == plan.FileKey && uploaded.ToUserID == peer &&
		uploaded.AESKeyHex == plan.AESKeyHex && uploaded.SHA256 == plan.SHA256 && uploaded.RawSize == plan.RawSize &&
		uploaded.CipherSize == plan.CipherSize && uploaded.Type == plan.Type && uploaded.Name == plan.Name && uploaded.MIME == plan.MIME && uploaded.MD5 == plan.MD5 && uploaded.DownloadParam != ""
}

func bridgeCheckLocalFile(ctx context.Context, file *weixin.MediaFile, dir string) error {
	if !bridgeFileRecordValid(file) {
		return errors.New("private original metadata validation failed")
	}
	if filepath.Clean(filepath.Dir(file.Path)) != filepath.Clean(dir) {
		return errors.New("private original path is outside media directory")
	}
	info, err := os.Lstat(file.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != file.Size {
		return errors.New("private original size or permission validation failed")
	}
	f, err := os.Open(file.Path)
	if err != nil {
		return errors.New("private original is unavailable")
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, bridgeContextReader{ctx, f}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("private original verification interrupted")
	}
	if hex.EncodeToString(digest.Sum(nil)) != file.SHA256 {
		return errors.New("private original hash validation failed")
	}
	return nil
}

type bridgeContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r bridgeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func bridgeDownloadNative(ctx context.Context, client bridgeNativeMedia, room, fileID, dir string) (*weixin.MediaFile, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot create private media directory")
	}
	stat, err := os.Stat(dir)
	if err != nil || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("media directory must be private")
	}
	f, err := os.CreateTemp(dir, "dot-original-*")
	if err != nil {
		return nil, errors.New("cannot stage native attachment")
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			os.Remove(f.Name())
		}
	}()
	digest := sha256.New()
	info, err := client.DownloadAttachmentTo(ctx, room, fileID, io.MultiWriter(f, digest))
	if err != nil {
		return nil, err
	}
	if info == nil || info.Name == "" || info.MIME == "" || info.Size < 0 {
		return nil, errors.New("native attachment metadata validation failed")
	}
	actual, err := f.Stat()
	if err != nil || actual.Size() != info.Size {
		return nil, errors.New("native attachment size validation failed")
	}
	if err := f.Sync(); err != nil {
		return nil, errors.New("cannot persist native attachment")
	}
	if err := f.Close(); err != nil {
		return nil, errors.New("cannot close native attachment")
	}
	typeID := weixin.FileItemType
	if strings.HasPrefix(info.MIME, "image/") {
		typeID = weixin.ImageItemType
	} else if strings.HasPrefix(info.MIME, "video/") {
		typeID = weixin.VideoItemType
	}
	complete = true
	return &weixin.MediaFile{Path: f.Name(), Name: info.Name, MIME: info.MIME, Size: info.Size, SHA256: hex.EncodeToString(digest.Sum(nil)), Type: typeID}, nil
}

func (e *bridgeEngine) queueNative(m chatgpt.Message, edited bool, fingerprint string) error {
	key := "native:" + m.ID + ":" + fingerprint
	human := bridgeHumanNative(m, e.state)
	if m.Deleted != nil {
		if human {
			return e.queue(key, bridgeHumanNativeText(m, "", edited), &m)
		}
		return e.queue(key, bridgeNativeText(m, edited), &m)
	}
	var files []string
	unsupported := false
	task := false
	for _, raw := range m.Content.Attachments {
		var ref struct {
			Type     string `json:"type"`
			FileID   string `json:"file_id"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(raw, &ref) != nil {
			unsupported = true
			continue
		}
		if ref.Type == "file" && ref.FileID != "" {
			files = append(files, ref.FileID)
		} else {
			unsupported = true
			if ref.Type == "thread" {
				task = true
			}
		}
	}
	text := m.Content.Text
	if edited && text != "" && !human {
		text = "dot 更新：\n" + text
	}
	if unsupported {
		if text != "" {
			text += "\n\n"
		}
		if task {
			if human {
				text += "这条原消息附带原生任务卡。请打开 https://chatgpt.com/ 并进入你的 dot 查看状态或审批。微信文字不会替代原生审批。"
			} else {
				text += "dot 发来了原生任务卡。请打开 https://chatgpt.com/ 并进入你的 dot 查看状态或审批。微信文字不会替代原生审批。"
			}
		} else {
			text += "这条消息含当前微信入口尚不能展示的交互，请在 dot 原界面查看。"
		}
	}
	if human {
		text = bridgeHumanNativeText(m, text, edited)
	}
	if text != "" || len(files) == 0 {
		if text == "" {
			text = bridgeNativeText(m, edited)
		}
		if err := e.queue(key, text, &m); err != nil {
			return err
		}
	}
	for _, fileID := range files {
		// Text/status revisions must not upload the same original again.
		fileKey := "native-file:" + m.ID + ":" + fileID
		known := false
		for _, out := range e.state.Outbound {
			if out.Key == fileKey {
				known = true
				break
			}
		}
		if known {
			continue
		}
		id, err := bridgeID()
		if err != nil {
			return err
		}
		clientID, err := bridgeID()
		if err != nil {
			return err
		}
		e.state.Outbound = append(e.state.Outbound, &bridgeOutbound{Key: fileKey, Native: &m, ClientID: clientID, Status: "ready", DeliveryKind: "media", Media: &bridgeOutgoingMedia{ID: id, NativeFileID: fileID, Stage: "received"}})
	}
	return nil
}

// v1 records preserve their original IDs, cursors, baseline and pending state.
// Already-reported unsupported messages are retained, not silently replayed.
func bridgeMigrateState(s *bridgeState) bool {
	if s.Version == 1 {
		s.Version = 2
		return true
	}
	return false
}

func (e *bridgeEngine) removePrivateMedia(path string) error {
	if path == "" {
		return nil
	}
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(e.mediaDir) {
		return errors.New("media cleanup path validation failed")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("private media cleanup failed; confirmed receipt retained")
	}
	return nil
}

func (e *bridgeEngine) cleanupAcknowledgedMedia() error {
	dirty := false
	for _, in := range e.state.Inbound {
		if in.Status != "acknowledged" {
			continue
		}
		for _, media := range in.Media {
			if media.Cleaned {
				continue
			}
			if media.File == nil {
				return errors.New("confirmed incoming media has no original receipt")
			}
			if err := e.removePrivateMedia(media.File.Path); err != nil {
				return err
			}
			media.Cleaned = true
			dirty = true
		}
	}
	for _, out := range e.state.Outbound {
		media := out.Media
		if out.Status != "acknowledged" || media == nil || media.Cleaned {
			continue
		}
		if media.File == nil {
			return errors.New("confirmed outgoing media has no original receipt")
		}
		if err := e.removePrivateMedia(media.File.Path); err != nil {
			return err
		}
		if media.Plan != nil {
			if err := e.removePrivateMedia(media.Plan.CipherPath); err != nil {
				return err
			}
		}
		media.Cleaned = true
		dirty = true
	}
	if dirty {
		return e.store()
	}
	return nil
}

func (e *bridgeEngine) shutdownMedia() {
	e.mediaWorkers.Wait()
	for {
		select {
		case result := <-e.mediaResults:
			// Only GET and local preparation can leave unreferenced artifacts.
			// Upload/send journals retain their original/plan for reconciliation.
			if result.Job.Phase == "downloading" && result.File != nil {
				_ = e.removePrivateMedia(result.File.Path)
			}
			if result.Job.Phase == "preparing" && result.Plan != nil {
				_ = e.removePrivateMedia(result.Plan.CipherPath)
			}
		default:
			return
		}
	}
}
