package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Battery233/sim-harbor/internal/diagnostics"
	"github.com/Battery233/sim-harbor/internal/protocol"
)

type voicemailSlotState struct {
	Slot int     `json:"slot"`
	Mode *int    `json:"mode"`
	TTS  *string `json:"tts"`
}

type voicemailBoardState struct {
	AMRURL  string               `json:"amr_url"`
	Channel string               `json:"channel"`
	Slots   []voicemailSlotState `json:"slots"`
}

func (s *Server) handleVoicemailBoard(w http.ResponseWriter, r *http.Request, action, intent string, confirmed bool, seconds *int, expectedRaw json.RawMessage, smsPhone, smsPassword string) {
	if seconds != nil {
		reply(w, 400, map[string]string{"error": "invalid_board_action"})
		return
	}
	if action == "voicemail_query" {
		if confirmed || intent != "" || len(expectedRaw) > 0 && string(expectedRaw) != "null" || smsPhone != "" || smsPassword != "" {
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
	// SMS reads leave mode and TTS nil. That means TCP did not return them, not that a restore snapshot of 0 and "" has drifted.
	if json.Unmarshal(expectedRaw, &expected) != nil || !voicemailObservationAgrees(current, expected) {
		reply(w, 409, map[string]string{"error": "board_parameter_changed"})
		return
	}
	target, ok := s.voicemailTarget(intent, current)
	if !ok {
		reply(w, 400, map[string]string{"error": "invalid_board_action"})
		return
	}
	smsSubmitted := false
	if !sameVoicemailState(current, target) {
		if needsVoicemailSMS(current, target) && (!boardPhone.MatchString(strings.TrimSpace(smsPhone)) || !validBoardPassword(smsPassword)) {
			reply(w, 409, map[string]string{"error": "voicemail_sms_required"})
			return
		}
		smsSubmitted, problem = s.writeVoicemailBoard(r.Context(), sess, intent, strings.TrimSpace(smsPhone), smsPassword, current, target)
		if problem != "" {
			if problem == "voicemail_sms_rejected" {
				reply(w, 502, map[string]string{"error": problem})
				return
			}
			reply(w, 502, map[string]string{"error": "board_setting_unknown"})
			return
		}
	}
	verified, problem := s.readBackVoicemailBoard(r.Context(), sess, target)
	if problem != "" || !sameVoicemailState(verified, target) {
		reply(w, 502, map[string]string{"error": "board_setting_unknown"})
		return
	}
	s.Logs.Record("board_parameter_verified", diagnostics.Fields{Session: sess.diagnosticID})
	reply(w, 200, map[string]any{"voicemail": verified, "verified": true, "unchanged": sameVoicemailState(current, target), "sms_submitted": smsSubmitted})
}

func (s *Server) voicemailTarget(intent string, current voicemailBoardState) (voicemailBoardState, bool) {
	target := current
	target.Slots = append([]voicemailSlotState(nil), current.Slots...)
	touch := map[int]bool{}
	if len(s.Config.VoicemailSlots) > 0 {
		for _, slot := range s.Config.VoicemailSlots {
			touch[slot] = true
		}
	} else if intent == "restore" && current.Channel != "sms" {
		for _, slot := range current.Slots {
			if slot.Mode == nil || *slot.Mode != 0 || slot.TTS == nil || *slot.TTS != "" {
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
				mode := 2
				target.Slots[i].Mode = &mode
			}
		}
	case "restore":
		target.AMRURL = ""
		for i, slot := range target.Slots {
			if touch[slot.Slot] {
				mode, text := 0, ""
				target.Slots[i].Mode = &mode
				target.Slots[i].TTS = &text
			}
		}
	default:
		return voicemailBoardState{}, false
	}
	return target, true
}

func (s *Server) writeVoicemailBoard(ctx context.Context, sess *session, intent, phone, password string, current, target voicemailBoardState) (bool, string) {
	type tcpStep struct {
		cmd    string
		sub    int
		params map[string]string
	}
	type smsStep struct {
		slot int
		body string
		ok   func(string) bool
	}
	var tcp []tcpStep
	var messages []smsStep
	unconfirmed := false
	modeWrites := func() {
		for _, slot := range target.Slots {
			cur := slotByNumber(current, slot.Slot)
			if sameOptionalInt(cur.Mode, slot.Mode) {
				continue
			}
			if current.Channel == "sms" {
				body, ok := voicemailSMSBody(slot.Slot, password, "mode", *slot.Mode, "")
				if !ok {
					messages = append(messages, smsStep{})
					return
				}
				mode := *slot.Mode
				n := slot.Slot
				messages = append(messages, smsStep{slot: n, body: body, ok: func(text string) bool { return smsModeOK(text, n, mode) }})
				continue
			}
			messages = nil
			tcp = append(tcp, tcpStep{"slotcim", -1, map[string]string{"p1": strconv.Itoa(slot.Slot), "p2": strconv.Itoa(*slot.Mode)}})
		}
	}
	textWrites := func() {
		for _, slot := range target.Slots {
			cur := slotByNumber(current, slot.Slot)
			if sameOptionalString(cur.TTS, slot.TTS) {
				continue
			}
			if current.Channel == "sms" {
				body, ok := voicemailSMSBody(slot.Slot, password, "tts", 0, *slot.TTS)
				if !ok {
					messages = append(messages, smsStep{})
					return
				}
				clear := *slot.TTS == ""
				n := slot.Slot
				messages = append(messages, smsStep{slot: n, body: body, ok: func(text string) bool { return smsTTSOK(text, n, clear) }})
				continue
			}
			tcp = append(tcp, tcpStep{"slottts", -1, map[string]string{"p1": strconv.Itoa(slot.Slot), "p2": *slot.TTS}})
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
		tcp = append(tcp, tcpStep{"setamrurl", 55, map[string]string{"p1": value}})
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
	for _, item := range messages {
		if item.body == "" || item.ok == nil {
			return false, "board_setting_unknown"
		}
	}
	runTCP := func(items []tcpStep) string {
		for _, item := range items {
			if _, problem := s.boardTextCommand(ctx, sess, item.cmd, item.sub, item.params); problem != "" {
				return problem
			}
		}
		return ""
	}
	runSMS := func() string {
		for _, item := range messages {
			confirmed, problem := s.boardSMSCommand(ctx, sess, item.slot, phone, item.body, item.ok)
			if problem != "" {
				return problem
			}
			if !confirmed {
				unconfirmed = true
			}
		}
		return ""
	}
	if intent == "restore" {
		if problem := runSMS(); problem != "" {
			return false, problem
		}
		return unconfirmed, runTCP(tcp)
	}
	if problem := runTCP(tcp); problem != "" {
		return false, problem
	}
	if problem := runSMS(); problem != "" {
		return false, problem
	}
	return unconfirmed, ""
}

func (s *Server) readVoicemailBoard(ctx context.Context, sess *session) (voicemailBoardState, string) {
	url, problem := s.boardTextCommand(ctx, sess, "askamrurl", 56, nil)
	if problem != "" {
		return voicemailBoardState{}, problem
	}
	parsedURL, ok := parseAMRURL(url.event.Val)
	if !ok {
		return voicemailBoardState{}, "board_reply_invalid"
	}
	state := voicemailBoardState{AMRURL: parsedURL, Channel: "tcp", Slots: []voicemailSlotState{{Slot: 1}, {Slot: 2}}}
	modes, problem := s.boardTextCommand(ctx, sess, "slotcim", -1, nil)
	if problem == "device_query_failed" {
		state.Channel = "sms"
		s.Logs.Record("voicemail_sms_channel", diagnostics.Fields{Session: sess.diagnosticID})
		return state, ""
	}
	if problem != "" {
		return voicemailBoardState{}, problem
	}
	parsed, ok := parseSlotModes(modes.event.Val)
	if !ok {
		return voicemailBoardState{}, "board_reply_invalid"
	}
	for i, slot := range state.Slots {
		mode := parsed[slot.Slot]
		state.Slots[i].Mode = &mode
		reply, problem := s.boardTextCommand(ctx, sess, "slottts", -1, map[string]string{"p1": strconv.Itoa(slot.Slot)})
		if problem != "" {
			return voicemailBoardState{}, problem
		}
		text, ok := parseSlotTTS(reply.event.Val)
		if !ok {
			return voicemailBoardState{}, "board_reply_invalid"
		}
		state.Slots[i].TTS = &text
	}
	return state, ""
}

func (s *Server) readBackVoicemailBoard(ctx context.Context, sess *session, target voicemailBoardState) (voicemailBoardState, string) {
	if target.Channel != "sms" {
		return s.readVoicemailBoard(ctx, sess)
	}
	url, problem := s.boardTextCommand(ctx, sess, "askamrurl", 56, nil)
	if problem != "" {
		return voicemailBoardState{}, problem
	}
	parsedURL, ok := parseAMRURL(url.event.Val)
	if !ok {
		return voicemailBoardState{}, "board_reply_invalid"
	}
	verified := target
	verified.Slots = append([]voicemailSlotState(nil), target.Slots...)
	verified.AMRURL = parsedURL
	return verified, ""
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

func (s *Server) boardSMSCommand(ctx context.Context, sess *session, slot int, phone, body string, accept func(string) bool) (bool, string) {
	s.mu.Lock()
	same := s.active == sess
	s.mu.Unlock()
	if !same {
		return false, "device_connection_changed"
	}
	id := tid()
	ch := make(chan queryReply, 4)
	watch := make(chan protocol.Event, 8)
	sess.mu.Lock()
	sess.tid = id
	sess.response = ch
	sess.smsWatch = watch
	sess.acceptSMSReport = true
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		if sess.tid == id {
			sess.tid = ""
			sess.response = nil
			sess.smsWatch = nil
			sess.acceptSMSReport = false
		}
		sess.mu.Unlock()
	}()
	s.Logs.Record("voicemail_sms_command", diagnostics.Fields{Session: sess.diagnosticID, Transaction: diagnostics.Reference(id), Slot: slot, SubType: 16})
	if err := s.write(sess, map[string]any{"cmd": "sendsms", "p1": strconv.Itoa(slot), "p2": phone, "p3": body, "tid": id}); err != nil {
		return false, "device_write_failed"
	}
	// X32v158 often never sends the type 501 control text. A sendsms accept
	// (401/402/502/503) or this short wait is enough; a 501 is optional evidence.
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	sawAccept := false
	submitted := false
	for {
		select {
		case result := <-ch:
			s.mu.Lock()
			same = s.active == sess
			s.mu.Unlock()
			if !same {
				return false, "device_connection_changed"
			}
			e := result.event
			if e.Code != nil && *e.Code != 0 {
				return false, "device_query_failed"
			}
			switch e.Type {
			case 401:
				sawAccept = true
			case 402, 502, 503:
				sawAccept = true
				if !submitted {
					submitted = true
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(time.Second)
				}
			}
		case ev := <-watch:
			if voicemailCommandSMS(ev) {
				continue
			}
			compact := compactSMS(ev.Body)
			if strings.Contains(compact, "密码不正确") || strings.Contains(compact, "格式错误") {
				return false, "voicemail_sms_rejected"
			}
			if accept(ev.Body) {
				return true, ""
			}
		case <-timer.C:
			outcome := "device_result_unknown"
			if sawAccept {
				outcome = "accepted"
			}
			s.Logs.Record("voicemail_sms_submitted", diagnostics.Fields{Session: sess.diagnosticID, Transaction: diagnostics.Reference(id), Slot: slot, Outcome: outcome})
			return false, ""
		case <-ctx.Done():
			return false, "device_result_unknown"
		}
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
	if len(out) != 2 {
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

func needsVoicemailSMS(current, target voicemailBoardState) bool {
	if current.Channel != "sms" {
		return false
	}
	for _, slot := range []int{1, 2} {
		left, right := slotByNumber(current, slot), slotByNumber(target, slot)
		if !sameOptionalInt(left.Mode, right.Mode) || !sameOptionalString(left.TTS, right.TTS) {
			return true
		}
	}
	return false
}

func voicemailObservationAgrees(current, expected voicemailBoardState) bool {
	if current.AMRURL != expected.AMRURL || current.Channel != expected.Channel || len(current.Slots) != 2 || len(expected.Slots) != 2 {
		return false
	}
	for _, slot := range []int{1, 2} {
		left, right := slotByNumber(current, slot), slotByNumber(expected, slot)
		if left.Slot != slot || right.Slot != slot || !unknownIntAgrees(left.Mode, right.Mode) || !unknownStringAgrees(left.TTS, right.TTS) {
			return false
		}
	}
	return true
}

func unknownIntAgrees(a, b *int) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

func unknownStringAgrees(a, b *string) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

func sameVoicemailState(a, b voicemailBoardState) bool {
	if a.AMRURL != b.AMRURL || a.Channel != b.Channel || len(a.Slots) != 2 || len(b.Slots) != 2 {
		return false
	}
	for _, slot := range []int{1, 2} {
		left, right := slotByNumber(a, slot), slotByNumber(b, slot)
		if left.Slot != slot || right.Slot != slot || !sameOptionalInt(left.Mode, right.Mode) || !sameOptionalString(left.TTS, right.TTS) {
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

func sameOptionalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

var boardPhone = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

func validBoardPassword(password string) bool {
	if len(password) < 4 || len(password) > 20 {
		return false
	}
	for _, r := range password {
		if r <= 32 || r > 126 || strings.ContainsRune("#@&~", r) {
			return false
		}
	}
	return true
}

func voicemailSMSBody(slot int, password, kind string, mode int, tts string) (string, bool) {
	if (slot != 1 && slot != 2) || !validBoardPassword(password) {
		return "", false
	}
	switch kind {
	case "mode":
		if mode != 0 && mode != 2 {
			return "", false
		}
		return "*#slot" + strconv.Itoa(slot) + "cim#" + password + "#" + strconv.Itoa(mode), true
	case "tts":
		if strings.ContainsAny(tts, "#@&~\x00\r\n") {
			return "", false
		}
		return "*#slot" + strconv.Itoa(slot) + "tts#" + password + "#" + tts, true
	default:
		return "", false
	}
}

func compactSMS(body string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, body)
}

func smsModeOK(body string, slot, mode int) bool {
	text := compactSMS(body)
	if !strings.Contains(text, "卡槽"+strconv.Itoa(slot)) || !strings.Contains(text, "设置为来电") {
		return false
	}
	switch mode {
	case 0:
		return strings.Contains(text, "不接听")
	case 2:
		if strings.Contains(text, "不接听") {
			return false
		}
		return strings.Contains(text, "接听并录音") || strings.Contains(text, "自动挂断") || strings.Contains(text, "自动接听")
	default:
		return false
	}
}

func smsTTSOK(body string, slot int, clear bool) bool {
	text := compactSMS(body)
	if !strings.Contains(text, "卡槽"+strconv.Itoa(slot)+"的自动语音") {
		return false
	}
	if clear {
		return strings.Contains(text, "已经取消")
	}
	return strings.Contains(text, "已经设置完成")
}

func voicemailCommandSMS(e protocol.Event) bool {
	for _, chunk := range smsChunks(e) {
		folded := strings.ToLower(strings.TrimSpace(chunk))
		for _, prefix := range []string{"*#slot1cim#", "*#slot2cim#", "*#slot1tts#", "*#slot2tts#", "*#slottts#", "*#slotts#", "*#slotcim#"} {
			if strings.Contains(folded, prefix) {
				return true
			}
		}
	}
	return false
}

func voicemailControlSMS(e protocol.Event) bool {
	if voicemailCommandSMS(e) {
		return true
	}
	text := compactSMS(e.Body)
	if strings.Contains(text, "用户密码不正确") || strings.Contains(text, "指令格式错误") {
		return true
	}
	if strings.Contains(text, "自动语音") && (strings.Contains(text, "已经设置完成") || strings.Contains(text, "已经取消")) {
		return true
	}
	return strings.Contains(text, "设置为来电")
}

func smsChunks(e protocol.Event) []string {
	chunks := []string{}
	if e.Body != "" {
		chunks = append(chunks, e.Body)
	}
	for _, part := range e.Fragments {
		_, text, ok := strings.Cut(part, ",")
		if ok {
			chunks = append(chunks, text)
		} else if part != "" {
			chunks = append(chunks, part)
		}
	}
	return chunks
}
