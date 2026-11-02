package copilot

import (
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

// terminalResult: InvalidRequest + message phải đọc đúng, không nuốt.
func TestTerminalResultInvalidRequest(t *testing.T) {
	item := map[string]any{
		"result":   map[string]any{"value": "InvalidRequest", "message": "Sorry, I wasn't able to respond."},
		"messages": []any{},
	}
	value, message := terminalResult(item)
	if value != "InvalidRequest" || !strings.Contains(message, "Sorry") {
		t.Fatalf("value=%q message=%q", value, message)
	}
	if err := terminalError(value, message); err == nil || !strings.Contains(err.Error(), "rejected continuation") {
		t.Fatalf("expected continuation error, got %v", err)
	}
	if !isInvalidRequestError(terminalError(value, message)) {
		t.Fatal("isInvalidRequestError must match continuation rejection")
	}
}

// Throttled phải phân loại riêng, không lẫn InvalidRequest.
func TestTerminalErrorThrottled(t *testing.T) {
	err := terminalError("Throttled", "slow down")
	if err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("expected throttled error, got %v", err)
	}
	if isInvalidRequestError(err) {
		t.Fatal("throttled must not classify as invalid request")
	}
}

// Empty Success vẫn là lỗi: handler sẽ không emit HTTP 200 rỗng.
func TestTerminalSuccessEmptyRejected(t *testing.T) {
	if err := terminalError("Success", ""); err == nil {
		t.Fatal("empty success message path must still be checked by caller")
	}
	if !isSuccessResult("Success") || !isSuccessResult("") {
		t.Fatal("Success and empty must pass")
	}
	if isSuccessResult("InvalidRequest") {
		t.Fatal("InvalidRequest must not pass")
	}
}

// Copilot upstream supports continuation when ConversationID and ChatSessionID
// are preserved while request IDs are regenerated.
func TestUpstreamContinuationEnabled(t *testing.T) {
	up := provider.UpstreamState{
		ConversationID: "c123", ChatSessionID: "s456",
	}
	sess := sessionFromUpstream(up)
	if sess.ConversationID != "c123" || sess.ChatSessionID != "s456" {
		t.Fatalf("session must preserve upstream IDs, got %+v", sess)
	}
	back := upstreamFromSession(sess)
	if back.ConversationID != "c123" || back.ChatSessionID != "s456" {
		t.Fatalf("upstream state mismatch: %+v", back)
	}
}

// captureTurnState nhận cả string và object.
func TestCaptureTurnStateShapes(t *testing.T) {
	var s Session
	captureTurnState(&s, `raw-state`)
	if s.TurnState != "raw-state" {
		t.Fatalf("turn=%q", s.TurnState)
	}
	captureTurnState(&s, map[string]any{"a": 1})
	if !strings.Contains(s.TurnState, `"a"`) {
		t.Fatalf("turn=%q", s.TurnState)
	}
	captureTransferToken(&s, "t")
	if s.ConversationTransferToken != "t" {
		t.Fatalf("token=%q", s.ConversationTransferToken)
	}
	captureTransferToken(&s, "")
	if s.ConversationTransferToken != "t" {
		t.Fatal("empty token must not overwrite")
	}
}

// TokenExpiresSoon: token 1h báo hết trong lead 60m... ngược lại token 3h không.
func TestTokenExpiresSoon(t *testing.T) {
	path := "/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000"
	soon := testTokenURL("substrate.office.com", path, time.Now().Add(30*time.Minute).Unix())
	if !TokenExpiresSoon(soon, time.Hour) {
		t.Fatal("30m token must expire soon within 60m lead")
	}
	fresh := testTokenURL("substrate.office.com", path, time.Now().Add(3*time.Hour).Unix())
	if TokenExpiresSoon(fresh, time.Hour) {
		t.Fatal("3h token must not expire soon within 60m lead")
	}
	if !TokenExpiresSoon("not-a-token", time.Hour) {
		t.Fatal("unparseable token must refresh conservatively")
	}
}
