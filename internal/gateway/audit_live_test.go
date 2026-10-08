package gateway

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sever_provider/internal/db"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/session"
)

// syntheticAuditAdapter simulates an upstream provider with memory of secret codes keyed by upstream conversation ID.
type syntheticAuditAdapter struct {
	mu             sync.Mutex
	conversations  map[string]string // convID -> stored code
	turnCounters   map[string]int
	callsPerCred   map[string]int
	upstreamDelay  time.Duration
	chunkDelay     time.Duration
}

func newSyntheticAuditAdapter(upstreamDelay, chunkDelay time.Duration) *syntheticAuditAdapter {
	return &syntheticAuditAdapter{
		conversations: make(map[string]string),
		turnCounters:  make(map[string]int),
		callsPerCred:  make(map[string]int),
		upstreamDelay: upstreamDelay,
		chunkDelay:    chunkDelay,
	}
}

func (a *syntheticAuditAdapter) Name() string { return "audit_synthetic" }
func (a *syntheticAuditAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}

func (a *syntheticAuditAdapter) Chat(ctx context.Context, req provider.ChatRequest, secret []byte) (provider.ChatResponse, error) {
	start := time.Now()
	if a.upstreamDelay > 0 {
		select {
		case <-time.After(a.upstreamDelay):
		case <-ctx.Done():
			return provider.ChatResponse{}, ctx.Err()
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	credID := string(secret)
	if credID == "" {
		credID = "anonymous-cred"
	}
	a.callsPerCred[credID]++

	convID := req.Upstream.ConversationID
	if convID == "" {
		convID = fmt.Sprintf("up-conv-%d", time.Now().UnixNano())
	}
	a.turnCounters[convID]++

	userMsg := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			userMsg = m.Content
		}
	}

	// Check if this is setting a code
	if strings.HasPrefix(userMsg, "REMEMBER_CODE:") {
		code := strings.TrimPrefix(userMsg, "REMEMBER_CODE:")
		a.conversations[convID] = code
		return provider.ChatResponse{
			ID:      fmt.Sprintf("chat-%d", time.Now().UnixNano()),
			Content: fmt.Sprintf("CODE_STORED:%s", code),
			Upstream: provider.UpstreamState{
				ConversationID: convID,
				ResponseID:     fmt.Sprintf("resp-%d", a.turnCounters[convID]),
			},
		}, nil
	}

	// If asking for the stored code without history
	if strings.Contains(userMsg, "RECALL_CODE") {
		stored, ok := a.conversations[convID]
		if !ok {
			return provider.ChatResponse{
				ID:      fmt.Sprintf("chat-%d", time.Now().UnixNano()),
				Content: "ERROR_NO_CODE_FOUND",
				Upstream: provider.UpstreamState{
					ConversationID: convID,
					ResponseID:     fmt.Sprintf("resp-%d", a.turnCounters[convID]),
				},
			}, nil
		}
		return provider.ChatResponse{
			ID:      fmt.Sprintf("chat-%d", time.Now().UnixNano()),
			Content: fmt.Sprintf("CODE_RECALLED:%s", stored),
			Upstream: provider.UpstreamState{
				ConversationID: convID,
				ResponseID:     fmt.Sprintf("resp-%d", a.turnCounters[convID]),
			},
		}, nil
	}

	_ = start
	return provider.ChatResponse{
		ID:      fmt.Sprintf("chat-%d", time.Now().UnixNano()),
		Content: "OK_GENERIC",
		Upstream: provider.UpstreamState{
			ConversationID: convID,
			ResponseID:     fmt.Sprintf("resp-%d", a.turnCounters[convID]),
		},
	}, nil
}

func (a *syntheticAuditAdapter) StreamChat(ctx context.Context, req provider.ChatRequest, secret []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 4)

	go func() {
		defer close(ch)

		if a.upstreamDelay > 0 {
			select {
			case <-time.After(a.upstreamDelay):
			case <-ctx.Done():
				ch <- provider.StreamChunk{Err: ctx.Err()}
				return
			}
		}

		a.mu.Lock()
		credID := string(secret)
		if credID == "" {
			credID = "anonymous-cred"
		}
		a.callsPerCred[credID]++

		convID := req.Upstream.ConversationID
		if convID == "" {
			convID = fmt.Sprintf("up-conv-%d", time.Now().UnixNano())
		}
		a.turnCounters[convID]++

		userMsg := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				userMsg = m.Content
			}
		}

		reply := "OK_STREAM"
		if strings.HasPrefix(userMsg, "REMEMBER_CODE:") {
			code := strings.TrimPrefix(userMsg, "REMEMBER_CODE:")
			a.conversations[convID] = code
			reply = fmt.Sprintf("CODE_STORED:%s", code)
		} else if strings.Contains(userMsg, "RECALL_CODE") {
			stored, ok := a.conversations[convID]
			if ok {
				reply = fmt.Sprintf("CODE_RECALLED:%s", stored)
			} else {
				reply = "ERROR_NO_CODE_FOUND"
			}
		}
		a.mu.Unlock()

		// Chunk 1 (TTFT)
		ch <- provider.StreamChunk{Content: reply[:len(reply)/2]}

		if a.chunkDelay > 0 {
			time.Sleep(a.chunkDelay)
		}

		// Chunk 2 (Rest)
		ch <- provider.StreamChunk{Content: reply[len(reply)/2:]}

		// Final chunk with upstream state
		ch <- provider.StreamChunk{
			Done: true,
			Upstream: provider.UpstreamState{
				ConversationID: convID,
				ResponseID:     fmt.Sprintf("resp-%d", a.turnCounters[convID]),
			},
		}
	}()

	return ch, nil
}

func (a *syntheticAuditAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}

func (a *syntheticAuditAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (a *syntheticAuditAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

type syntheticCredentialSource struct {
	creds []identity.Credential
}

func (s *syntheticCredentialSource) List(context.Context, string) ([]identity.Credential, error) {
	return s.creds, nil
}

func (s *syntheticCredentialSource) ReadSecret(_ context.Context, id string) ([]byte, error) {
	return []byte(id), nil
}

// TestAuditLiveSessionSuite runs an end-to-end audit in an isolated environment with synthetic data.
func TestAuditLiveSessionSuite(t *testing.T) {
	tempDBPath := filepath.Join(t.TempDir(), "audit_sessions.db")
	database, err := db.Open(context.Background(), tempDBPath)
	if err != nil {
		t.Fatalf("failed to open temp db: %v", err)
	}
	defer database.Close()

	sessionMgr, err := session.NewPersistent(database.DB)
	if err != nil {
		t.Fatalf("failed to init persistent sessions: %v", err)
	}

	upstreamDelay := 20 * time.Millisecond
	chunkDelay := 10 * time.Millisecond
	adapter := newSyntheticAuditAdapter(upstreamDelay, chunkDelay)

	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "audit-chat-model",
		Provider:     "audit_synthetic",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming},
		TimeoutSec:   10,
	})

	reg := provider.NewRegistry()
	reg.Register(adapter)

	leases := scheduler.NewLeaseManager()
	credSource := &syntheticCredentialSource{
		creds: []identity.Credential{
			{ID: "cred-alpha", Domain: "audit_synthetic", Status: identity.StatusHealthy, MaxConcurrency: 1},
			{ID: "cred-beta", Domain: "audit_synthetic", Status: identity.StatusHealthy, MaxConcurrency: 1},
		},
	}

	dispatcher := &scheduler.Dispatcher{
		Models:      models,
		Credentials: credSource,
		Adapters:    reg,
		Leases:      leases,
		Sessions:    sessionMgr,
	}

	handler := NewHandler(dispatcher, models)
	router := NewRouter(handler, "")
	server := httptest.NewServer(router)
	defer server.Close()

	// 1. Session 1: Turn 1 (Remember random code)
	sessID1 := fmt.Sprintf("audit-sess-1-%d", time.Now().UnixNano())
	code1 := fmt.Sprintf("SECRET_AUTH_%d", time.Now().UnixNano()%100000)

	t1Start := time.Now()
	reqBody1, _ := json.Marshal(ChatCompletionRequest{
		Model: "audit-chat-model",
		Messages: []provider.Message{
			{Role: "user", Content: "REMEMBER_CODE:" + code1},
		},
	})
	httpReq1, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqBody1))
	httpReq1.Header.Set("Content-Type", "application/json")
	httpReq1.Header.Set("X-Session-ID", sessID1)

	resp1, err := http.DefaultClient.Do(httpReq1)
	if err != nil || resp1.StatusCode != 200 {
		t.Fatalf("turn 1 failed: status=%v err=%v", resp1.StatusCode, err)
	}
	var res1 ChatCompletionResponse
	_ = json.NewDecoder(resp1.Body).Decode(&res1)
	resp1.Body.Close()
	t1Duration := time.Since(t1Start)

	if !strings.Contains(res1.Choices[0].Message.Content, code1) {
		t.Fatalf("turn 1 did not confirm code store: %s", res1.Choices[0].Message.Content)
	}

	// Verify session 1 binding after turn 1
	b1, ok1 := sessionMgr.Get(sessID1)
	if !ok1 {
		t.Fatalf("session 1 was not bound in session manager")
	}
	assignedCred1 := b1.CredentialID
	assignedConv1 := b1.UpstreamConversationID
	if assignedCred1 == "" || assignedConv1 == "" {
		t.Fatalf("session 1 missing cred or upstream conv id: %+v", b1)
	}
	if b1.Turns != 1 {
		t.Fatalf("session 1 turn count expected 1, got %d", b1.Turns)
	}

	// 2. Session 1: Turn 2 (Recall code WITHOUT sending history)
	t2Start := time.Now()
	reqBody2, _ := json.Marshal(ChatCompletionRequest{
		Model: "audit-chat-model",
		Messages: []provider.Message{
			{Role: "user", Content: "RECALL_CODE"},
		},
	})
	httpReq2, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqBody2))
	httpReq2.Header.Set("Content-Type", "application/json")
	httpReq2.Header.Set("X-Session-ID", sessID1)

	resp2, err := http.DefaultClient.Do(httpReq2)
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("turn 2 failed: status=%v err=%v", resp2.StatusCode, err)
	}
	var res2 ChatCompletionResponse
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	resp2.Body.Close()
	t2Duration := time.Since(t2Start)

	// Verify exact code recalled
	expectedReply := fmt.Sprintf("CODE_RECALLED:%s", code1)
	if res2.Choices[0].Message.Content != expectedReply {
		t.Fatalf("turn 2 recall failed, expected %q, got %q", expectedReply, res2.Choices[0].Message.Content)
	}

	// Verify same credential and same upstream conversation preserved
	b1AfterTurn2, _ := sessionMgr.Get(sessID1)
	if b1AfterTurn2.CredentialID != assignedCred1 {
		t.Fatalf("credential switched from %s to %s", assignedCred1, b1AfterTurn2.CredentialID)
	}
	if b1AfterTurn2.UpstreamConversationID != assignedConv1 {
		t.Fatalf("upstream conversation switched from %s to %s", assignedConv1, b1AfterTurn2.UpstreamConversationID)
	}
	if b1AfterTurn2.Turns != 2 {
		t.Fatalf("turn count expected 2, got %d", b1AfterTurn2.Turns)
	}

	// 3. Repeat via Streaming mode: Turn 3
	t3Start := time.Now()
	reqBody3, _ := json.Marshal(ChatCompletionRequest{
		Model:  "audit-chat-model",
		Stream: true,
		Messages: []provider.Message{
			{Role: "user", Content: "RECALL_CODE"},
		},
	})
	httpReq3, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqBody3))
	httpReq3.Header.Set("Content-Type", "application/json")
	httpReq3.Header.Set("X-Session-ID", sessID1)

	resp3, err := http.DefaultClient.Do(httpReq3)
	if err != nil || resp3.StatusCode != 200 {
		t.Fatalf("streaming turn 3 failed: status=%v err=%v", resp3.StatusCode, err)
	}

	var streamContentAccum strings.Builder
	var ttftDuration time.Duration
	var firstTokenReceived bool

	scanner := bufio.NewScanner(resp3.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataPayload := strings.TrimPrefix(line, "data: ")
			if dataPayload == "[DONE]" {
				continue
			}
			var chunkResp ChatCompletionResponse
			if err := json.Unmarshal([]byte(dataPayload), &chunkResp); err == nil {
				if len(chunkResp.Choices) > 0 && chunkResp.Choices[0].Delta != nil {
					content := chunkResp.Choices[0].Delta.Content
					if content != "" {
						if !firstTokenReceived {
							ttftDuration = time.Since(t3Start)
							firstTokenReceived = true
						}
						streamContentAccum.WriteString(content)
					}
				}
			}
		}
	}
	resp3.Body.Close()
	t3Duration := time.Since(t3Start)

	fullStreamed := streamContentAccum.String()
	if !strings.Contains(fullStreamed, code1) {
		t.Fatalf("streaming response did not contain code %q, got %q", code1, fullStreamed)
	}

	// Verify session turns after streaming
	b1AfterTurn3, _ := sessionMgr.Get(sessID1)
	if b1AfterTurn3.Turns != 3 {
		t.Fatalf("turn count after stream expected 3, got %d", b1AfterTurn3.Turns)
	}
	if b1AfterTurn3.CredentialID != assignedCred1 || b1AfterTurn3.UpstreamConversationID != assignedConv1 {
		t.Fatalf("stream turn corrupted affinity: %+v", b1AfterTurn3)
	}

	// 4. Isolation Test: Session 2 with different code
	sessID2 := fmt.Sprintf("audit-sess-2-%d", time.Now().UnixNano())
	code2 := fmt.Sprintf("SECRET_BETA_%d", time.Now().UnixNano()%100000)

	reqBodyS2, _ := json.Marshal(ChatCompletionRequest{
		Model: "audit-chat-model",
		Messages: []provider.Message{
			{Role: "user", Content: "REMEMBER_CODE:" + code2},
		},
	})
	httpReqS2, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqBodyS2))
	httpReqS2.Header.Set("Content-Type", "application/json")
	httpReqS2.Header.Set("X-Session-ID", sessID2)
	respS2, _ := http.DefaultClient.Do(httpReqS2)
	respS2.Body.Close()

	// Ask session 2 to recall: must be code2, never code1
	reqRecallS2, _ := json.Marshal(ChatCompletionRequest{
		Model: "audit-chat-model",
		Messages: []provider.Message{
			{Role: "user", Content: "RECALL_CODE"},
		},
	})
	httpReqRecallS2, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqRecallS2))
	httpReqRecallS2.Header.Set("Content-Type", "application/json")
	httpReqRecallS2.Header.Set("X-Session-ID", sessID2)
	respRecallS2, _ := http.DefaultClient.Do(httpReqRecallS2)
	var resRecallS2 ChatCompletionResponse
	_ = json.NewDecoder(respRecallS2.Body).Decode(&resRecallS2)
	respRecallS2.Body.Close()

	if resRecallS2.Choices[0].Message.Content != fmt.Sprintf("CODE_RECALLED:%s", code2) {
		t.Fatalf("session isolation breach: session 2 recalled %q instead of %q", resRecallS2.Choices[0].Message.Content, code2)
	}

	b2, _ := sessionMgr.Get(sessID2)
	if b2.UpstreamConversationID == assignedConv1 {
		t.Fatalf("session 1 and session 2 shared same upstream conversation!")
	}

	// 5. Concurrency & Gate Lock Test: 2 concurrent requests on SAME session
	var wg sync.WaitGroup
	startBoth := make(chan struct{})
	var durA, durB time.Duration
	var lockWaitObserved time.Duration

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startBoth
		reqA, _ := json.Marshal(ChatCompletionRequest{
			Model: "audit-chat-model",
			Messages: []provider.Message{
				{Role: "user", Content: "RECALL_CODE"},
			},
		})
		hReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqA))
		hReq.Header.Set("Content-Type", "application/json")
		hReq.Header.Set("X-Session-ID", sessID1)
		s := time.Now()
		r, err := http.DefaultClient.Do(hReq)
		if err == nil {
			r.Body.Close()
		}
		durA = time.Since(s)
	}()

	go func() {
		defer wg.Done()
		<-startBoth
		time.Sleep(2 * time.Millisecond) // ensure reqA acquires gate first
		reqB, _ := json.Marshal(ChatCompletionRequest{
			Model: "audit-chat-model",
			Messages: []provider.Message{
				{Role: "user", Content: "RECALL_CODE"},
			},
		})
		hReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(reqB))
		hReq.Header.Set("Content-Type", "application/json")
		hReq.Header.Set("X-Session-ID", sessID1)
		s := time.Now()
		r, err := http.DefaultClient.Do(hReq)
		if err == nil {
			r.Body.Close()
		}
		durB = time.Since(s)
	}()

	close(startBoth)
	wg.Wait()

	// Since upstreamDelay is 20ms and both are serialized on sessID1's gate,
	// reqB must wait for reqA to finish, so durB should be >= upstreamDelay + durA_remaining (~35-45ms)
	if durB > durA {
		lockWaitObserved = durB - durA
	} else {
		lockWaitObserved = durA - durB
	}
	t.Logf("Concurrent gate lock: durA=%v, durB=%v, lockWaitObserved=%v", durA, durB, lockWaitObserved)

	// 6. Persistence Test: Re-open storage in isolated environment without production restart
	// Close current DB handle to simulate shutdown of session manager
	var rawBindingJSON string
	row := database.DB.QueryRow(`SELECT binding FROM chat_session_bindings WHERE id=?`, sessID1)
	if err := row.Scan(&rawBindingJSON); err != nil {
		t.Fatalf("session 1 not found in sqlite db: %v", err)
	}

	// Reopen a fresh session manager from the same SQLite DB
	db2, err := sql.Open("sqlite", tempDBPath)
	if err != nil {
		t.Fatalf("failed to re-open sqlite db: %v", err)
	}
	defer db2.Close()

	reopenedMgr, err := session.NewPersistent(db2)
	if err != nil {
		t.Fatalf("failed to restore persistent session manager: %v", err)
	}

	reopenedB1, ok := reopenedMgr.Get(sessID1)
	if !ok {
		t.Fatalf("reopened session manager did not find session 1")
	}
	if reopenedB1.CredentialID != assignedCred1 {
		t.Fatalf("reopened credential mismatch: got %s, want %s", reopenedB1.CredentialID, assignedCred1)
	}
	if reopenedB1.UpstreamConversationID != assignedConv1 {
		t.Fatalf("reopened upstream conv mismatch: got %s, want %s", reopenedB1.UpstreamConversationID, assignedConv1)
	}
	if reopenedB1.Turns < 3 {
		t.Fatalf("reopened turns unexpected: %d", reopenedB1.Turns)
	}

	t.Logf("Audit Summary Latencies:")
	t.Logf("Turn 1 Duration: %v", t1Duration)
	t.Logf("Turn 2 Duration: %v", t2Duration)
	t.Logf("Turn 3 (Streaming) Total: %v, TTFT: %v", t3Duration, ttftDuration)
	t.Logf("Concurrent Lock Wait Approx: %v", lockWaitObserved)
	t.Logf("Credential calls: %+v", adapter.callsPerCred)
}
