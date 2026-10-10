// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
)

func bridgeHumanNative(m chatgpt.Message, s *bridgeState) bool {
	return s.User != "" && m.Sender == s.User
}

// The authenticated user must be the sole non-Dot member of this personal DM.
// The existing native verification supplies the room and Dot identity.
func bridgeVerifyHumanMember(room *chatgpt.Room, human, dotSender string) error {
	if room == nil || human == "" || dotSender == "" || human == dotSender || len(room.Members) != 2 {
		return errors.New("dot room Human member identity mismatch; reconnect required")
	}
	humans, dots := 0, 0
	for _, member := range room.Members {
		switch member.ID {
		case human:
			humans++
		case dotSender:
			dots++
		default:
			return errors.New("dot room Human member identity mismatch; reconnect required")
		}
	}
	if humans != 1 || dots != 1 {
		return errors.New("dot room Human member identity mismatch; reconnect required")
	}
	return nil
}

// Suppress only the unchanged original that this bridge sent from Weixin.
// An edit or deletion in the original UI remains a new visible revision.
func bridgeWeixinNativeEcho(m chatgpt.Message, s *bridgeState) bool {
	if !bridgeHumanNative(m, s) || m.Deleted != nil {
		return false
	}
	// Returning to the WX payload after an original-UI edit is another edit,
	// not the initial echo. Saved native revisions also retain unknown outcomes.
	for _, out := range s.Outbound {
		if out.Native != nil && out.Native.ID == m.ID && bridgeHumanNative(*out.Native, s) {
			return false
		}
	}
	for _, in := range s.Inbound {
		sameID := in.NativeID != "" && in.NativeID == m.ID
		sameRequest := in.RequestID != "" && in.RequestID == m.RequestID
		if !sameID && !sameRequest {
			continue
		}
		text, files := bridgeInboundNativePayload(in)
		if m.Content.Text == text && bridgeNativeFilesMatch(&m, files) {
			return true
		}
	}
	return false
}

func bridgeNativeQueued(m chatgpt.Message, fingerprint string, s *bridgeState) bool {
	for _, out := range s.Outbound {
		if out.Native != nil && out.Native.ID == m.ID && bridgeNativeFingerprint(*out.Native) == fingerprint {
			return true
		}
	}
	return false
}

func bridgeNativeHasPrior(m chatgpt.Message, s *bridgeState) bool {
	for _, out := range s.Outbound {
		if out.Native != nil && out.Native.ID == m.ID {
			return true
		}
	}
	for _, in := range s.Inbound {
		if in.NativeID != "" && in.NativeID == m.ID {
			return true
		}
	}
	return false
}

func bridgeNativeInHistory(m chatgpt.Message, s *bridgeState) bool {
	for _, out := range s.Outbound {
		if bridgeHistoryEntryValid(out, s) && bridgeHistoryMessageID(out, s) == m.ID {
			return true
		}
	}
	return false
}

func bridgeNativeHistoryUnresolved(m chatgpt.Message, s *bridgeState) bool {
	for _, out := range s.Outbound {
		if (out.Status == "unknown" || out.Status == "attempting") &&
			bridgeHistoryEntryValid(out, s) && bridgeHistoryMessageID(out, s) == m.ID {
			return true
		}
	}
	return false
}

func bridgeHumanNativeText(m chatgpt.Message, text string, edited bool) string {
	label := "你在 dot"
	if m.Deleted != nil {
		text = "你已在 dot 删除这条消息。微信中的原消息暂无法撤回，请以 dot 原界面为准。"
	} else if edited {
		label += "（更新）"
	}
	originalTime := "原时间未提供"
	if !m.Created.IsZero() {
		originalTime = m.Created.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02 15:04:05.999999999 CST")
	}
	return "【" + label + "｜" + originalTime + "】\n" + text
}
