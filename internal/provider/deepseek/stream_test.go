package deepseek

import (
	"context"
	"strings"
	"testing"
)

func TestStreamParserThinkingAndContent(t *testing.T) {
	sseInput := `
data: {"v":{"response":{"fragments":[{"id":1,"type":"THINK","content":"Suy nghĩ ban đầu"}]}}}

data: {"p":"response/fragments/-1/content","o":"APPEND","v":" và phân tích"}

data: {"p":"response/fragments","o":"APPEND","v":[{"id":2,"type":"RESPONSE","content":"Kết quả là: "}]}

data: {"v":"42"}

data: {"p":"response/status","o":"SET","v":"FINISHED"}

data: [DONE]
`

	parser := NewStreamParser(true, "sess-123")
	stream := parser.ParseStream(context.Background(), strings.NewReader(sseInput))

	var thinking strings.Builder
	var content strings.Builder
	doneReceived := false

	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		if chunk.ReasoningContent != "" {
			thinking.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
		}
		if chunk.Done {
			doneReceived = true
		}
	}

	if !doneReceived {
		t.Fatal("expected Done signal, but did not receive it")
	}
	wantThinking := "Suy nghĩ ban đầu và phân tích"
	if thinking.String() != wantThinking {
		t.Fatalf("thinking = %q, want %q", thinking.String(), wantThinking)
	}
	wantContent := "Kết quả là: 42"
	if content.String() != wantContent {
		t.Fatalf("content = %q, want %q", content.String(), wantContent)
	}
}

func TestStreamParserThinkCloseTagTransition(t *testing.T) {
	sseInput := `
data: {"v":"Đang suy nghĩ... "}

data: {"v":"hoàn tất suy nghĩ.</think>Đây là câu trả lời."}

data: {"v":" Thêm một câu nữa."}

data: [DONE]
`

	parser := NewStreamParser(true, "sess-tag")
	stream := parser.ParseStream(context.Background(), strings.NewReader(sseInput))

	var thinking strings.Builder
	var content strings.Builder

	for chunk := range stream {
		if chunk.ReasoningContent != "" {
			thinking.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
		}
	}

	wantThinking := "Đang suy nghĩ... hoàn tất suy nghĩ."
	if thinking.String() != wantThinking {
		t.Fatalf("thinking = %q, want %q", thinking.String(), wantThinking)
	}
	wantContent := "Đây là câu trả lời. Thêm một câu nữa."
	if content.String() != wantContent {
		t.Fatalf("content = %q, want %q", content.String(), wantContent)
	}
}

func TestStripThinkingTags(t *testing.T) {
	raw := "<think>Suy nghĩ bí mật</think>Câu trả lời chính thức"
	cleaned := StripThinkingTags(raw)
	want := "Suy nghĩ bí mậtCâu trả lời chính thức"
	if cleaned != want {
		t.Fatalf("StripThinkingTags = %q, want %q", cleaned, want)
	}
}
