package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Battery233/sim-harbor/internal/diagnostics"
)

type voicemailSlotState struct {
	Slot int    `json:"slot"`
	Mode int    `json:"mode"`
	TTS  string `json:"tts"`
}

type voicemailBoardState struct {
	AMRURL string               `json:"amr_url"`
	Slots  []voicemailSlotState `json:"slots"`
}

func (s *Server) handleVoicemailBoard(w http.ResponseWriter, r *http.Request, action, intent string, confirmed bool, seconds *int, expectedRaw json.RawMessage) {
	if seconds != nil {
		reply(w, 400, map[string]string{"error": "invalid_board_action"})
		return
	}
	if action == "voicemail_query" {
		if confirmed || intent != "" || len(expectedRaw) > 0 && string(expectedRaw) != "null" {
			reply(w, 400, map[string]string{"error": "invalid_board_action"})
			return
		}
	} else {
		if !confirmed || (intent != "enable" && intent != "restore") || len(expectedRaw) == 0 {
			reply(w, 400, map[string]string{"error": "invalid_board_action"})
			return
		}
		if intent == "enable" && !s.Config.VoicemailEnabled {
			reply(w, 409, map[string]string{"error": "voicemail_disabled"})
			return
		}
	}
	if !s.sendGate.TryLock() {
		reply(w, 409, map[string]string{"error": "send_in_progress"})
		return
	}
	defer s.sendGate.Unlock()
	if !s.query.TryLock() {
		reply(w, 409, map[string]string{"error": "query_in_progress"})
		return
	}
	defer s.query.Unlock()
	s.mu.Lock()
	sess := s.active
	s.mu.Unlock()
	if sess == nil {
		reply(w, 409, map[string]string{"error": "device_offline"})
		return
	}
	current, problem := s.readVoicemailBoard(r.Context(), sess)
	if problem != "" {
		reply(w, 502, map[string]string{"error": problem})
		return
	}
	if action == "voicemail_query" {
		reply(w, 200, map[string]any{"voicemail": current})
		return
	}
	var expected voicemailBoardState
	if json.Unmarshal(expectedRaw, &expected) != nil || !sameVoicemailState(current, expected) {
		reply(w, 409, map[string]string{"error": "board_parameter_changed"})
		return
	}
	target, ok := s.voicemailTarget(intent, current)
	if !ok {
		reply(w, 400, map[string]string{"error": "invalid_board_action"})
		return
	}
	if !sameVoicemailState(current, target) {
		if problem = s.writeVoicemailBoard(r.Context(), sess, intent, current, target); problem != "" {
			reply(w, 502, map[string]string{"error": "board_setting_unknown"})
			return
		}
	}
	verified, problem := s.readVoicemailBoard(r.Context(), sess)
	if problem != "" || !sameVoicemailState(verified, target) {
		reply(w, 502, map[string]string{"error": "board_setting_unknown"})
		return
	}
	s.Logs.Record("board_parameter_verified", diagnostics.Fields{Session: sess.diagnosticID})
	reply(w, 200, map[string]any{"voicemail": verified, "verified": true, "unchanged": sameVoicemailState(current, target)})
}

func (s *Server) voicemailTarget(intent string, current voicemailBoardState) (voicemailBoardState, bool) {
	target := current
	target.Slots = append([]voicemailSlotState(nil), current.Slots...)
	touch := map[int]bool{}
	if len(s.Config.VoicemailSlots) > 0 {
		for _, slot := range s.Config.VoicemailSlots {
			touch[slot] = true
		}
	} else if intent == "restore" {
		for _, slot := range current.Slots {
			if slot.Mode != 0 || slot.TTS != "" {
				touch[slot.Slot] = true
			}
		}
	} else {
		return voicemailBoardState{}, false
	}
	switch intent {
	case "enable":
		if !s.Config.VoicemailEnabled || s.Config.VoicemailAMRURL == "" || s.Config.VoicemailTTS == "" {
			return voicemailBoardState{}, false
		}
		target.AMRURL = s.Config.VoicemailAMRURL
		for i, slot := range target.Slots {
			if touch[slot.Slot] {
				target.Slots[i].Mode = 2
				target.Slots[i].TTS = s.Config.VoicemailTTS
			}
		}
	case "restore":
		target.AMRURL = ""
		for i, slot := range target.Slots {
			if touch[slot.Slot] {
				target.Slots[i].Mode = 0
				target.Slots[i].TTS = ""
			}
		}
	default:
		return voicemailBoardState{}, false
	}
	return target, true
}

func (s *Server) writeVoicemailBoard(ctx context.Context, sess *session, intent string, current, target voicemailBoardState) string {
	type step struct {
		cmd    string
		sub    int
		params map[string]string
	}
	var steps []step
	modeWrites := func() {
		for _, slot := range target.Slots {
			cur := slotByNumber(current, slot.Slot)
			if cur.Mode == slot.Mode {
				continue
			}
			steps = append(steps, step{"slotcim", -1, map[string]string{"p1": strconv.Itoa(slot.Slot), "p2": strconv.Itoa(slot.Mode)}})
		}
	}
	textWrites := func() {
		for _, slot := range target.Slots {
			cur := slotByNumber(current, slot.Slot)
			if cur.TTS == slot.TTS {
				continue
			}
			steps = append(steps, step{"slottts", -1, map[string]string{"p1": strconv.Itoa(slot.Slot), "p2": slot.TTS}})
		}
	}
	urlWrite := func() {
		if current.AMRURL == target.AMRURL {
			return
		}
		value := target.AMRURL
		if value == "" {
			value = "clear"
		}
		steps = append(steps, step{"setamrurl", 55, map[string]string{"p1": value}})
	}
	if intent == "restore" {
		modeWrites()
		textWrites()
		urlWrite()
	} else {
		urlWrite()
		textWrites()
		modeWrites()
	}
	for _, item := range steps {
		if _, problem := s.boardTextCommand(ctx, sess, item.cmd, item.sub, item.params); problem != "" {
			return problem
		}
	}
	return ""
}

func (s *Server) readVoicemailBoard(ctx context.Context, sess *session) (voicemailBoardState, string) {
	modes, problem := s.boardTextCommand(ctx, sess, "slotcim", -1, nil)
	if problem != "" {
		return voicemailBoardState{}, problem
	}
	parsed, ok := parseSlotModes(modes.event.Val)
	if !ok {
		return voicemailBoardState{}, "board_reply_invalid"
	}
	state := voicemailBoardState{Slots: []voicemailSlotState{{Slot: 1, Mode: parsed[1]}, {Slot: 2, Mode: parsed[2]}}}
	for i, slot := range state.Slots {
		reply, problem := s.boardTextCommand(ctx, sess, "slottts", -1, map[string]string{"p1": strconv.Itoa(slot.Slot)})
		if problem != "" {
			return voicemailBoardState{}, problem
		}
		text, ok := parseSlotTTS(reply.event.Val)
		if !ok {
			return voicemailBoardState{}, "board_reply_invalid"
		}
		state.Slots[i].TTS = text
	}
	url, problem := s.boardTextCommand(ctx, sess, "askamrurl", 56, nil)
	if problem != "" {
		return voicemailBoardState{}, problem
	}
	parsedURL, ok := parseAMRURL(url.event.Val)
	if !ok {
		return voicemailBoardState{}, "board_reply_invalid"
	}
	state.AMRURL = parsedURL
	return state, ""
}

func (s *Server) boardTextCommand(ctx context.Context, sess *session, cmd string, sub int, params map[string]string) (queryReply, string) {
	s.mu.Lock()
	same := s.active == sess
	s.mu.Unlock()
	if !same {
		return queryReply{}, "device_connection_changed"
	}
	id := tid()
	ch := make(chan queryReply, 1)
	sess.mu.Lock()
	sess.tid = id
	sess.response = ch
	sess.mu.Unlock()
	defer func() { sess.mu.Lock(); sess.tid = ""; sess.response = nil; sess.mu.Unlock() }()
	body := map[string]any{"cmd": cmd, "tid": id}
	for key, value := range params {
		body[key] = value
	}
	loggedSub := sub
	if loggedSub < 0 {
		loggedSub = 0
	}
	s.Logs.Record("board_parameter_command", diagnostics.Fields{Session: sess.diagnosticID, Transaction: diagnostics.Reference(id), SubType: loggedSub})
	if err := s.write(sess, body); err != nil {
		return queryReply{}, "device_write_failed"
	}
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	select {
	case result := <-ch:
		s.mu.Lock()
		same = s.active == sess
		s.mu.Unlock()
		if !same {
			return queryReply{}, "device_connection_changed"
		}
		e := result.event
		if e.Type != 402 || e.Code == nil || *e.Code != 0 || (sub >= 0 && int(e.SubType) != sub) {
			return queryReply{}, "device_query_failed"
		}
		return result, ""
	case <-timer.C:
		return queryReply{}, "device_result_unknown"
	case <-ctx.Done():
		return queryReply{}, "device_result_unknown"
	}
}

func parseSlotModes(value string) (map[int]int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ";")
	if len(parts) != 2 {
		return nil, false
	}
	labeled := strings.Contains(value, ":")
	out := map[int]int{}
	for i, part := range parts {
		part = strings.TrimSpace(part)
		slot, modeText := i+1, part
		if labeled {
			left, right, ok := strings.Cut(part, ":")
			n, err := strconv.Atoi(strings.TrimSpace(left))
			if !ok || err != nil {
				return nil, false
			}
			slot, modeText = n, strings.TrimSpace(right)
		}
		mode, err := strconv.Atoi(modeText)
		if err != nil || slot < 1 || slot > 2 || mode < 0 || mode > 3 {
			return nil, false
		}
		if _, exists := out[slot]; exists {
			return nil, false
		}
		out[slot] = mode
	}
	if out[1] < 0 || out[2] < 0 || len(out) != 2 {
		return nil, false
	}
	return out, true
}

func parseSlotTTS(value string) (string, bool) {
	if strings.ContainsAny(value, "\x00\r\n") || utf8.RuneCountInString(value) > 274 {
		return "", false
	}
	return value, true
}

func parseAMRURL(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\x00\r\n ") || len(value) > 1500 {
		return "", false
	}
	return value, true
}

func sameVoicemailState(a, b voicemailBoardState) bool {
	if a.AMRURL != b.AMRURL || len(a.Slots) != 2 || len(b.Slots) != 2 {
		return false
	}
	for _, slot := range []int{1, 2} {
		left, right := slotByNumber(a, slot), slotByNumber(b, slot)
		if left.Slot != slot || right.Slot != slot || left.Mode != right.Mode || left.TTS != right.TTS {
			return false
		}
	}
	return true
}

func slotByNumber(state voicemailBoardState, slot int) voicemailSlotState {
	for _, item := range state.Slots {
		if item.Slot == slot {
			return item
		}
	}
	return voicemailSlotState{}
}
