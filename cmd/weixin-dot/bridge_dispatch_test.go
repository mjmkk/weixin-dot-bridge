// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

func bridgeDispatchNative(id, text string) chatgpt.Message {
	m := chatgpt.Message{ID: id, Sender: "dot-member", Created: time.Now().UTC()}
	m.Content.Text = text
	return m
}

func TestBridgeDefaultHistoryPauseReservesContextForDelayedLiveReply(t *testing.T) {
	for _, status := range []string{"ready", "rejected", "unknown", "attempting"} {
		t.Run(status, func(t *testing.T) {
			e, n, w := bridgeTestEngine()
			history := &bridgeOutbound{Text: "original complete history", ClientID: "history-client", DeliveryKind: "history", Status: status,
				ContextToken: "old-context", Attempts: 1, LastFailure: &bridgeSendFailure{Op: "sendmessage", Ret: -2}}
			history.Key = "history:room:historical-id:" + bridgeTextSHA(history.Text)
			e.state.Outbound = []*bridgeOutbound{history}
			before := bridgeHash(history)
			if e.allowHistory {
				t.Fatal("normal chat enabled history without an explicit backfill request")
			}
			if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("new-input", "peer", "current request", "new-context")}}); err != nil {
				t.Fatal(err)
			}
			if err := e.deliverNative(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(n.sends) != 1 || len(w.sends) != 0 || bridgeHash(history) != before {
				t.Fatal("history consumed the new context while dot was still processing, or its records changed")
			}
			m := bridgeDispatchNative("new-reply", "the delayed current reply")
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(w.sends) != 1 || w.sends[0].Text != m.Content.Text || w.sends[0].ContextToken != "new-context" || bridgeHash(history) != before {
				t.Fatal("paused historical barrier blocked the real reply or was falsely acknowledged/replayed")
			}
		})
	}
}

func TestBridgeLiveKnownRejectionResumesSamePayloadOncePerNewContext(t *testing.T) {
	e, _, w := bridgeTestEngine()
	e.state.ContextToken = "old-context"
	m := bridgeDispatchNative("live-reply", "original current reply")
	if err := e.queue("live-key", m.Content.Text, &m); err != nil {
		t.Fatal(err)
	}
	out := e.state.Outbound[0]
	w.err = &weixin.APIError{Op: "sendmessage", Kind: weixin.ErrAPI, Ret: -2}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	key, text, clientID := out.Key, out.Text, out.ClientID
	failure := *out.LastFailure
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 1 || out.Status != "rejected" || out.Attempts != 1 {
		t.Fatal("the rejected live message retried with its old context")
	}
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("renewal", "peer", "new input", "new-context")}}); err != nil {
		t.Fatal(err)
	}
	var durable *bridgeState
	e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
	w.beforeSend = func(input weixin.SendTextRequest) {
		if durable == nil || durable.Outbound[0].Status != "attempting" || durable.Outbound[0].ContextToken != "new-context" ||
			durable.Outbound[0].Attempts != 2 || *durable.Outbound[0].LastFailure != failure ||
			input.Text != text || input.ClientID != clientID || durable.Outbound[0].Key != key {
			t.Fatal("live retry lacked a durable new context or changed the original delivery identity")
		}
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || out.Status != "rejected" || out.Attempts != 2 || out.ContextToken != "new-context" {
		t.Fatal("renewed context did not allow exactly one live retry or a new rejection did not freeze it")
	}
	w.beforeSend, w.err = nil, nil
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("renewal-2", "peer", "another input", "next-context")}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 3 || out.Status != "acknowledged" || out.Attempts != 3 || out.Key != key || out.Text != text || out.ClientID != clientID || *out.LastFailure != failure {
		t.Fatal("a later renewal failed to acknowledge the retained live payload or lost its prior evidence")
	}
}

func TestBridgeBlockedNativeRevisionDoesNotFreezeUnrelatedLiveMessages(t *testing.T) {
	for _, status := range []string{"unknown", "attempting", "rejected"} {
		t.Run(status, func(t *testing.T) {
			e, _, w := bridgeTestEngine()
			e.state.ContextToken = "current-context"
			original := bridgeDispatchNative("same-message", "original revision")
			edit := original
			edit.Content.Text = "later edited revision"
			independent := bridgeDispatchNative("other-message", "independent current reply")
			for i, m := range []*chatgpt.Message{&original, &edit, &independent} {
				if err := e.queue([]string{"original", "edit", "independent"}[i], m.Content.Text, m); err != nil {
					t.Fatal(err)
				}
			}
			blocked := e.state.Outbound[0]
			blocked.Status, blocked.ContextToken, blocked.Attempts = status, "current-context", 1
			blocked.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: -2}
			for i := 0; i < 2; i++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if len(w.sends) != 1 || w.sends[0].Text != independent.Content.Text || blocked.Attempts != 1 ||
				e.state.Outbound[1].Status != "ready" || e.state.Outbound[1].Attempts != 0 || e.state.Outbound[2].Status != "acknowledged" {
				t.Fatal("blocked POST was repeated, its later edit overtook it, or unrelated live messages froze")
			}
			if (status == "unknown" || status == "attempting") && blocked.Status != "unknown" {
				t.Fatal("uncertain native revision lost its unknown disposition")
			}
		})
	}
}

func TestBridgeUnknownHistoryBlocksSameNativeRevisionOnly(t *testing.T) {
	for _, status := range []string{"unknown", "attempting"} {
		for _, sender := range []string{"dot-member", "human-member"} {
			t.Run(status+"/"+sender, func(t *testing.T) {
				e, _, w := bridgeTestEngine()
				e.state.ContextToken, e.state.User = "current-context", "human-member"
				history := &bridgeOutbound{Text: "original historical payload", ClientID: "history-client", DeliveryKind: "history", Status: status,
					ContextToken: "old-context", Attempts: 1}
				history.Key = "history:room:same-message:" + bridgeTextSHA(history.Text)
				e.state.Outbound = append(e.state.Outbound, history)
				before := bridgeHash(history)
				edit := bridgeDispatchNative("same-message", "later native revision")
				edit.Sender = sender
				other := bridgeDispatchNative("other-message", "unrelated current reply")
				for i, m := range []*chatgpt.Message{&edit, &other} {
					if err := e.queue([]string{"edit", "other"}[i], m.Content.Text, m); err != nil {
						t.Fatal(err)
					}
				}
				for i := 0; i < 2; i++ {
					if err := e.deliverWeixin(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if bridgeHash(history) != before || len(w.sends) != 1 || w.sends[0].Text != other.Content.Text ||
					e.state.Outbound[1].Status != "ready" || e.state.Outbound[1].Attempts != 0 || e.state.Outbound[2].Status != "acknowledged" {
					t.Fatal("live revision overtook unknown history, history changed, or unrelated live delivery froze")
				}
			})
		}
	}
}

type bridgeDispatchMismatchedMediaACK struct{ *bridgeMediaWeixinFake }

func (f *bridgeDispatchMismatchedMediaACK) SendMedia(_ context.Context, input weixin.SendMediaRequest) (*weixin.SendResult, error) {
	f.mediaSends = append(f.mediaSends, input)
	return &weixin.SendResult{MessageID: "unmatched-ack", ClientID: "different-client"}, nil
}

func TestBridgeNewUnknownMediaDoesNotSpendContextOnEnabledHistory(t *testing.T) {
	for _, failure := range []string{"unknown-error", "mismatched-ack"} {
		t.Run(failure, func(t *testing.T) {
			e, n, w := bridgeTestMediaEngine(t)
			n.downloadSize = 16
			m := bridgeNativeWithFile("original-file", "")
			if err := e.queueNative(m, false, bridgeHash(m)); err != nil {
				t.Fatal(err)
			}
			for phase := 0; phase < 4; phase++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				bridgeFinishMedia(t, e)
			}
			history := &bridgeOutbound{Text: "retained historical message", ClientID: "history-client", DeliveryKind: "history", Status: "ready"}
			history.Key = "history:room:historical-id:" + bridgeTextSHA(history.Text)
			e.state.Outbound = append(e.state.Outbound, history)
			e.allowHistory = true
			before := bridgeHash(history)
			if failure == "unknown-error" {
				w.mediaSendErr = weixin.ErrOutcomeUnknown
			} else {
				e.weixin = &bridgeDispatchMismatchedMediaACK{w}
			}
			for i := 0; i < 2; i++ {
				if err := e.deliverWeixin(context.Background()); err != nil {
					t.Fatal(err)
				}
				if bridgeHash(history) != before {
					t.Fatal("history was sent while a newly unknown live media result was unresolved")
				}
			}
			if len(w.mediaSends) != 1 || e.state.Outbound[1].Status != "unknown" {
				t.Fatal("unknown media POST was replayed or declared acknowledged")
			}
		})
	}
}
