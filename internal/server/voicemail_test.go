package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Battery233/sim-harbor/internal/config"
	"github.com/Battery233/sim-harbor/internal/protocol"
)

func voicemailURL(token string) string {
	return "http://192.168.31.10:18880/api/voicemail/amr/" + token + "?slot={{slot}}&start={{telStartTs}}&phone={{phNum|$urlEncode()}}"
}

func enableVoicemail(c *config.Config, token string, slots ...int) {
	c.VoicemailEnabled = true
	c.VoicemailSlots = slots
	c.VoicemailTTS = "请在提示音后留言"
	c.VoicemailAMRURL = voicemailURL(token)
}

func TestVoicemailEnabledCallSendsNoCommandsAndSkipsMissedBark(t *testing.T) {
	payloads := make(chan map[string]any, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		payloads <- p
		w.Write([]byte(`{"code":200}`))
	}))
	defer receiver.Close()
	s, l := fixture(t, 1, func(c *config.Config) {
		c.BarkKey, c.BarkBaseURL, c.PublicURL = "SYNTHETIC", receiver.URL, "https://example.invalid"
		enableVoicemail(c, strings.Repeat("ab", 16), 2)
	})
	board := connect(t, l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunNotifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	start := time.Now().Unix()
	send(t, board, s.codec(), map[string]any{"devId": "SIM-TEST", "type": 601, "slot": 2, "iccId": "SYNTHETIC-ICC", "scName": "First name", "phNum": "SYNTHETIC-CALLER", "telStartTs": start, "telConnectedTs": 0})
	send(t, board, s.codec(), map[string]any{"type": 603, "slot": 2, "iccId": "SYNTHETIC-ICC", "scName": "First name", "phNum": "SYNTHETIC-CALLER", "telStartTs": start, "telConnectedTs": 0, "telEndTs": start + 8})
	deadline := time.Now().Add(time.Second)
	for {
		call, err := s.Store.Call(context.Background(), 1, start)
		if err == nil && call.State == "missed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("call not recorded", err)
		}
		time.Sleep(time.Millisecond)
	}
	var deadlineAt int64
	var jobs int
	if err := s.Store.DB.QueryRow(`SELECT timeout_at FROM calls WHERE id=1`).Scan(&deadlineAt); err != nil || deadlineAt != 0 {
		t.Fatal("voicemail slot armed missed timeout", deadlineAt, err)
	}
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM call_notifications`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("missed notification queued", jobs, err)
	}
	select {
	case p := <-payloads:
		t.Fatal("missed bark on voicemail slot", p["title"])
	case <-time.After(200 * time.Millisecond):
	}
	board.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if p, err := protocol.NewReader(board).Next(); err == nil {
		t.Fatal("voicemail call sent a device command", len(p))
	}
	if w := action(s, "/calls/1/answer", map[string]bool{"confirmed": true}); w.Code != 404 && w.Code != 405 {
		t.Fatal("call control route", w.Code)
	}
}

func TestVoicemailUploadStoresLinksAndBarksOnce(t *testing.T) {
	payloads := make(chan map[string]any, 3)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		payloads <- p
		w.Write([]byte(`{"code":200}`))
	}))
	defer receiver.Close()
	token := strings.Repeat("cd", 16)
	s, l := fixture(t, 1, func(c *config.Config) {
		c.AdminToken = strings.Repeat("T", 32)
		c.BarkKey, c.BarkBaseURL, c.PublicURL = "SYNTHETIC", receiver.URL, "https://example.invalid"
		enableVoicemail(c, token, 2)
	})
	board := connect(t, l)
	start := time.Now().Unix()
	send(t, board, s.codec(), map[string]any{"devId": "SIM-TEST", "type": 601, "slot": 2, "iccId": "SYNTHETIC-ICC", "scName": "First name", "phNum": "SYNTHETIC-CALLER", "telStartTs": start})
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := s.Store.Call(context.Background(), 1, start); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing call")
		}
		time.Sleep(time.Millisecond)
	}
	audio := []byte("#!AMR\nSYNTHETIC")
	if w := postVoicemail(s, token, audio, start, "192.0.2.1:9"); w.Code != 403 {
		t.Fatal("outside source accepted", w.Code, w.Body)
	}
	if w := postVoicemail(s, strings.Repeat("ee", 16), audio, start, "127.0.0.1:9"); w.Code != 401 {
		t.Fatal("wrong token", w.Code)
	}
	w := postVoicemail(s, token, audio, start, "127.0.0.1:9")
	var saved map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved["errcode"] != float64(0) || saved["media_id"] == "" || saved["type"] != "amr" {
		t.Fatal(w.Code, w.Body)
	}
	again := postVoicemail(s, token, audio, start, "127.0.0.1:9")
	var duplicate map[string]any
	json.Unmarshal(again.Body.Bytes(), &duplicate)
	if again.Code != 200 || duplicate["media_id"] != saved["media_id"] {
		t.Fatal("duplicate changed media id", again.Code, again.Body)
	}
	var rows int
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM voicemails`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("duplicate stored twice", rows, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunNotifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case p := <-payloads:
		body, _ := p["body"].(string)
		if p["title"] != "First name 有留言" || !strings.Contains(body, "有留言") || strings.Contains(body, "未接") || strings.Contains(body, "响铃") || !strings.HasSuffix(p["url"].(string), "/?call=1") {
			t.Fatal("wrong voicemail bark", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("voicemail bark absent")
	}
	select {
	case p := <-payloads:
		t.Fatal("second bark", p["title"])
	case <-time.After(200 * time.Millisecond):
	}
	down := request(s, "GET", "http://example.com/api/calls/1/voicemail", "Bearer "+s.Config.AdminToken, "")
	if down.Code != 200 || down.Header().Get("Content-Type") != "audio/amr" || !strings.Contains(down.Header().Get("Content-Disposition"), "attachment;") || down.Body.String() != string(audio) {
		t.Fatal("download", down.Code, down.Header(), down.Body)
	}
	if open := request(s, "GET", "http://example.com/api/calls/1/voicemail", "", ""); open.Code != 401 {
		t.Fatal("download without credential", open.Code)
	}
	board.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if p, err := protocol.NewReader(board).Next(); err == nil {
		t.Fatal("upload path sent a device command", len(p))
	}
}

func TestVoicemailUploadRejectsWhenDisabledOrAllowlistOff(t *testing.T) {
	token := strings.Repeat("ab", 16)
	s, _ := fixture(t, 1)
	if w := postVoicemail(s, token, []byte("#!AMR\nSYNTHETIC"), 1700000000, "127.0.0.1:9"); w.Code != 404 {
		t.Fatal("disabled upload", w.Code)
	}
	off := false
	s, _ = fixture(t, 1, func(c *config.Config) {
		enableVoicemail(c, token, 1)
		c.DeviceAllowlistEnabled = &off
	})
	if w := postVoicemail(s, token, []byte("#!AMR\nSYNTHETIC"), 1700000000, "127.0.0.1:9"); w.Code != 403 {
		t.Fatal("allowlist off accepted upload", w.Code)
	}
}

func TestVoicemailSettingsConfirmAndBoardWhitelist(t *testing.T) {
	s := settingsFixture(t)
	snap, err := s.Settings.Snapshot()
	if err != nil || snap.Values.VoicemailEnabled {
		t.Fatal("default on", err)
	}
	token := strings.Repeat("ab", 16)
	values := snap.Values
	values.VoicemailEnabled = true
	values.VoicemailSlots = []int{1}
	values.VoicemailTTS = "请留言"
	values.VoicemailAMRURL = "http://lab.example.nip.io:18880/api/voicemail/amr/" + token
	body := map[string]any{"values": values, "secrets": map[string]any{}, "revision": snap.Revision, "confirmed": true, "voicemail_confirmed": true}
	if w := settingsRequest(s, "PUT", "/api/settings", body, true); w.Code != 422 {
		t.Fatal("public name accepted", w.Code, w.Body)
	}
	values.VoicemailAMRURL = voicemailURL(token)
	body["values"] = values
	body["voicemail_confirmed"] = false
	if w := settingsRequest(s, "PUT", "/api/settings", body, true); w.Code != 400 || !strings.Contains(w.Body.String(), "voicemail_confirmation_required") {
		t.Fatal("enable without secondary confirm", w.Code, w.Body)
	}
	body["voicemail_confirmed"] = true
	w := settingsRequest(s, "PUT", "/api/settings", body, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var saved config.SettingsSnapshot
	json.Unmarshal(w.Body.Bytes(), &saved)
	body["revision"] = saved.Revision
	body["voicemail_confirmed"] = false
	if w = settingsRequest(s, "PUT", "/api/settings", body, true); w.Code != 200 {
		t.Fatal("already enabled required another confirm", w.Code, w.Body)
	}
	if s.Config.VoicemailEnabled {
		t.Fatal("save changed runtime before restart")
	}

	runtime, listener := fixture(t, 1, func(c *config.Config) { enableVoicemail(c, token, 1) })
	if w := action(runtime, "/board/parameters", map[string]any{"action": "voicemail_apply", "intent": "enable", "confirmed": true, "expected": map[string]any{"amr_url": "", "slots": []any{}}}); w.Code != 409 {
		t.Fatal("offline apply", w.Code)
	}
	disabled, boardListener := fixture(t, 1)
	board := connect(t, boardListener)
	send(t, board, disabled.codec(), map[string]any{"type": 998, "devId": "SIM-TEST"})
	waitActive(t, disabled)
	if w := action(disabled, "/board/parameters", map[string]any{"action": "voicemail_apply", "intent": "enable", "confirmed": true, "expected": idleVoicemail()}); w.Code != 409 || !strings.Contains(w.Body.String(), "voicemail_disabled") {
		t.Fatal("disabled apply", w.Code, w.Body)
	}
	board.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if p, err := protocol.NewReader(board).Next(); err == nil {
		t.Fatal("disabled apply sent a command", len(p))
	}

	board = connect(t, listener)
	send(t, board, runtime.codec(), map[string]any{"type": 998, "devId": "SIM-TEST"})
	waitActive(t, runtime)
	board.SetDeadline(time.Now().Add(20 * time.Second))
	reader := protocol.NewReader(board)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- action(runtime, "/board/parameters", map[string]any{"action": "voicemail_apply", "intent": "enable", "confirmed": true, "expected": driftedVoicemail()})
	}()
	for i := 0; i < 4; i++ {
		cmd := read(t, reader, runtime.codec())
		if cmd["cmd"] == "telanswer" || cmd["cmd"] == "telstartrecord" || cmd["cmd"] == "teldial" || cmd["cmd"] == "telhangup" {
			t.Fatal("call control command", cmd["cmd"])
		}
		val := ""
		if cmd["cmd"] == "slotcim" {
			val = "1:1;2:0"
		}
		replyBoard(t, board, runtime, cmd, val)
	}
	if w := <-done; w.Code != 409 {
		t.Fatal("drift accepted", w.Code, w.Body)
	}
	board.SetDeadline(time.Now().Add(20 * time.Second))
	board.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := reader.Next(); err == nil {
		t.Fatal("mismatch continued into a write")
	}
	board.SetDeadline(time.Now().Add(20 * time.Second))

	go func() {
		done <- action(runtime, "/board/parameters", map[string]any{"action": "voicemail_apply", "intent": "enable", "confirmed": true, "expected": idleVoicemail()})
	}()
	seen := []string{}
	for i := 0; i < 4; i++ {
		cmd := read(t, reader, runtime.codec())
		seen = append(seen, cmd["cmd"].(string))
		if cmd["p2"] != nil || (cmd["cmd"] == "slotcim" && cmd["p1"] != nil) {
			t.Fatal("query included a write field", cmd)
		}
		replyBoard(t, board, runtime, cmd, idleReply(cmd))
	}
	for i := 0; i < 3; i++ {
		cmd := read(t, reader, runtime.codec())
		seen = append(seen, cmd["cmd"].(string))
		if cmd["cmd"] == "setamrurl" && cmd["p1"] != runtime.Config.VoicemailAMRURL {
			t.Fatal("upload url", cmd["p1"])
		}
		if cmd["cmd"] == "slottts" && (cmd["p1"] != "1" || cmd["p2"] != runtime.Config.VoicemailTTS) {
			t.Fatal("tts write", cmd)
		}
		if cmd["cmd"] == "slotcim" && (cmd["p1"] != "1" || cmd["p2"] != "2") {
			t.Fatal("mode write", cmd)
		}
		if cmd["cmd"] == "slotcim" && cmd["p1"] == "2" {
			t.Fatal("untouched slot written")
		}
		replyBoard(t, board, runtime, cmd, "ok")
	}
	for i := 0; i < 4; i++ {
		cmd := read(t, reader, runtime.codec())
		seen = append(seen, "readback:"+cmd["cmd"].(string))
		replyBoard(t, board, runtime, cmd, enabledReply(cmd, runtime.Config))
	}
	w = <-done
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"verified":true`) {
		t.Fatal(w.Code, w.Body, seen)
	}
	for _, cmd := range seen {
		if strings.Contains(cmd, "tel") {
			t.Fatal(cmd)
		}
	}
}

func idleVoicemail() map[string]any {
	return map[string]any{"amr_url": "", "slots": []map[string]any{{"slot": 1, "mode": 0, "tts": ""}, {"slot": 2, "mode": 0, "tts": ""}}}
}
func driftedVoicemail() map[string]any {
	return map[string]any{"amr_url": "", "slots": []map[string]any{{"slot": 1, "mode": 0, "tts": ""}, {"slot": 2, "mode": 0, "tts": ""}}}
}
func idleReply(cmd map[string]any) string {
	switch cmd["cmd"] {
	case "slotcim":
		return "0;0"
	default:
		return ""
	}
}
func enabledReply(cmd map[string]any, cfg config.Config) string {
	switch cmd["cmd"] {
	case "slotcim":
		return "2;0"
	case "slottts":
		if cmd["p1"] == "1" {
			return cfg.VoicemailTTS
		}
		return ""
	case "askamrurl":
		return cfg.VoicemailAMRURL
	default:
		return ""
	}
}
func subFor(cmd map[string]any) int {
	switch cmd["cmd"] {
	case "setamrurl":
		return 55
	case "askamrurl":
		return 56
	default:
		return 7
	}
}
func replyBoard(t *testing.T, board net.Conn, s *Server, cmd map[string]any, val string) {
	t.Helper()
	send(t, board, s.codec(), map[string]any{"type": 402, "subType": subFor(cmd), "code": 0, "tid": cmd["tid"], "val": val})
}

func waitActive(t *testing.T, s *Server) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		active := s.active != nil
		s.mu.Unlock()
		if active {
			return
		}
		if time.Now().After(until) {
			t.Fatal("not bound")
		}
		time.Sleep(time.Millisecond)
	}
}

func postVoicemail(s *Server, token string, audio []byte, start int64, remote string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("media", "rec001.amr")
	part.Write(audio)
	w.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/voicemail/amr/"+token+"?slot=2&start="+strconv.FormatInt(start, 10)+"&phone=SYNTHETIC-CALLER", &buf)
	req.RemoteAddr = remote
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}
