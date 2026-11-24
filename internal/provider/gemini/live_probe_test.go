package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

// liveCookie loads the opt-in probe cookie; the harness skips without it.
func liveCookie(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("GEMINI_PROBE_COOKIE_FILE"))
	if path == "" {
		t.Skip("GEMINI_PROBE_COOKIE_FILE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookie: %v", err)
	}
	cookie, err := SanitizeCookie(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("sanitize cookie: %v", err)
	}
	return cookie
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func runLiveProbe(t *testing.T, adapter *Adapter, cookie, label, hex string, mode int, effort, prompt string) {
	t.Helper()
	id := "probe-" + label
	webModels[id] = webModelConfig{hex: hex, mode: mode, image: true}
	defer delete(webModels, id)

	secret := []byte(`{"cookie":` + quoteJSON(cookie) + `}`)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := adapter.Chat(ctx, provider.ChatRequest{
		Model:           id,
		Messages:        []provider.Message{{Role: "user", Content: prompt}},
		ReasoningEffort: effort,
	}, secret)
	elapsed := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		t.Logf("%-22s %7s ERROR %v", label, elapsed, err)
		return
	}
	text := strings.Join(strings.Fields(resp.Content), " ")
	runes := []rune(text)
	preview := text
	if len(runes) > 70 {
		preview = string(runes[:70])
	}
	t.Logf("%-22s %7s len=%3d reasoning=%3d %q", label, elapsed, len(runes), len([]rune(resp.ReasoningContent)), preview)
}

// TestLiveModelProbe checks which upstream ids and modes the backend accepts.
func TestLiveModelProbe(t *testing.T) {
	cookie := liveCookie(t)
	adapter := New(nil)
	const mathPrompt = "Nêu đúng 1 câu: 17 nhân 23 bằng bao nhiêu?"
	cases := []struct {
		label, hex string
		mode       int
	}{
		{"hydrogen-m1", "56fdd199312815e2", 1},
		{"lite-m6", "8c46e95b1a07cecc", 6},
		{"pro-m3", "e6fa609c3fa255c0", 3},
		{"argon-hex-m8", "95a4c0abcba24364", 8},
		{"argon-hex-m1", "95a4c0abcba24364", 1},
		{"argon-id-m8", "argon", 8},
		{"argon-id-m1", "argon", 1},
		{"carbon-797f-m3", "797f3d0293f288ad", 3},
	}
	for _, c := range cases {
		runLiveProbe(t, adapter, cookie, c.label, c.hex, c.mode, "", mathPrompt)
	}
}

// TestLiveThinkingDepth checks whether reasoning_effort changes the answer.
func TestLiveThinkingDepth(t *testing.T) {
	cookie := liveCookie(t)
	adapter := New(nil)
	const hardPrompt = "Một cửa hàng nhập 3 lô hàng, mỗi lô 48 hộp, mỗi hộp 12 gói. Bán 1/4 số gói với giá 15k, phần còn lại 18k. Tính tổng doanh thu. Trả lời gọn."
	for _, effort := range []string{"low", "medium", "high"} {
		runLiveProbe(t, adapter, cookie, "hydrogen-"+effort, "56fdd199312815e2", 1, effort, hardPrompt)
	}
}

// postRaw issues one StreamGenerate call and returns the raw body.
func postRaw(t *testing.T, adapter *Adapter, cookie, hex, effort, prompt string) []byte {
	return postRawMode(t, adapter, cookie, hex, 1, effort, prompt)
}

func postRawMode(t *testing.T, adapter *Adapter, cookie, hex string, mode int, effort, prompt string) []byte {
	t.Helper()
	probeID := "raw-" + strings.TrimPrefix(hex, "raw-")
	webModels[probeID] = webModelConfig{hex: hex, mode: mode}
	defer delete(webModels, probeID)
	adapter.syncTokenBaseURL()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tokens, err := adapter.Tokens.Get(ctx, cookie)
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	body := BuildPayloadWithThinking(prompt, hex, tokens.XSRF, "", ConversationMetadata{}, nil, effort)
	u, _ := url.Parse(adapter.BaseURL + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate")
	q := u.Query()
	q.Set("bl", tokens.BL)
	q.Set("hl", "en")
	q.Set("rt", "c")
	if tokens.FSID != "" {
		q.Set("f.sid", tokens.FSID)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers(cookie, SapisidFromCookie(cookie), time.Now()) {
		req.Header[k] = v
	}
	rpc, err := chatRPCHeader(probeID, effort)
	if err != nil {
		t.Fatalf("rpc header: %v", err)
	}
	req.Header.Set("x-goog-ext-525001261-jspb", rpc)
	resp, err := adapter.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return raw
}

// upstreamModelNames returns the inner[42] display names seen in a response.
func upstreamModelNames(raw []byte) []string {
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		var outer []any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &outer) != nil || len(outer) == 0 {
			continue
		}
		row, _ := outer[0].([]any)
		if len(row) < 3 {
			continue
		}
		innerText, _ := row[2].(string)
		var inner []any
		if json.Unmarshal([]byte(innerText), &inner) != nil || len(inner) <= 42 {
			continue
		}
		if name, _ := inner[42].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// TestLiveModelNames asks upstream which named model actually served a request:
// the response carries the display label in inner[42], so a hex+mode matrix can
// be resolved without driving the web UI.
func TestLiveModelNames(t *testing.T) {
	cookie := liveCookie(t)
	adapter := New(nil)
	hexes := []string{"56fdd199312815e2", "8c46e95b1a07cecc", "e6fa609c3fa255c0",
		"9d8ca3786ebdfbea", "797f3d0293f288ad", "95a4c0abcba24364"}
	modes := []int{1, 3, 6}
	for _, hex := range hexes {
		for _, mode := range modes {
			raw := postRawMode(t, adapter, cookie, hex, mode, "", "Nói OK")
			names := dedupeStrings(upstreamModelNames(raw))
			label := strings.Join(names, ", ")
			if label == "" {
				label = "(no label)"
			}
			t.Logf("%-16s mode=%d -> %s", hex, mode, label)
		}
	}
}

// TestLiveDumpRaw saves a raw payload so parser failures can be reproduced.
func TestLiveDumpRaw(t *testing.T) {
	dump := strings.TrimSpace(os.Getenv("GEMINI_PROBE_DUMP"))
	if dump == "" {
		t.Skip("GEMINI_PROBE_DUMP not set")
	}
	cookie := liveCookie(t)
	adapter := New(nil)
	const prompt = "Một cửa hàng nhập 3 lô hàng, mỗi lô 48 hộp, mỗi hộp 12 gói. Bán 1/4 số gói với giá 15k, phần còn lại 18k. Tính tổng doanh thu. Trả lời gọn."
	raw := postRaw(t, adapter, cookie, "56fdd199312815e2", "low", prompt)
	if err := os.WriteFile(dump, raw, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	ok, fail, thinking := 0, 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		text, thoughts, _, hasFrame := parseFrame(line)
		if hasFrame && strings.TrimSpace(text) != "" {
			ok++
		} else {
			fail++
		}
		if thoughts != "" {
			thinking++
		}
	}
	t.Logf("bytes=%d parsed=%d unparsed=%d with_thinking=%d saved=%s", len(raw), ok, fail, thinking, dump)
}

// TestLiveThinkingSlotProbe checks whether the thinking slot exists and keeps
// the documented shape candidate[37][0][0]. Only lengths are logged, never the
// chain content.
func TestLiveThinkingSlotProbe(t *testing.T) {
	cookie := liveCookie(t)
	adapter := New(nil)
	const hardPrompt = "Một cửa hàng nhập 3 lô hàng, mỗi lô 48 hộp, mỗi hộp 12 gói. Bán 1/4 số gói với giá 15k, phần còn lại 18k. Tính tổng doanh thu. Trả lời gọn."
	raw := postRawMode(t, adapter, cookie, "e6fa609c3fa255c0", 3, "high", hardPrompt)
	frames, withSlot, withText, thoughtsLen := 0, 0, 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		text, thoughts, _, hasFrame := parseFrame(line)
		if !hasFrame {
			continue
		}
		frames++
		if text != "" {
			withText++
		}
		if thoughts != "" {
			withSlot++
			if len(thoughts) > thoughtsLen {
				thoughtsLen = len(thoughts)
			}
		}
	}
	t.Logf("frames=%d with_text=%d with_thinking_slot=%d max_thoughts_len=%d extracted=%d",
		frames, withText, withSlot, thoughtsLen, len([]rune(ExtractResponseThoughts(string(raw)))))
}
