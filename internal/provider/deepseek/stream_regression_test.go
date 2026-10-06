package deepseek

import (
	"context"
	"strings"
	"testing"
)

func TestEmptyAndTruncatedStreamsFail(t *testing.T) {
	for _, input := range []string{"data: [DONE]\n\n", "data: {\"v\":\"partial\"}\n\n"} {
		var failed bool
		for chunk := range NewStreamParser(false, "s").ParseStream(context.Background(), strings.NewReader(input)) {
			if chunk.Done {
				t.Fatal("false success")
			}
			failed = failed || chunk.Err != nil
		}
		if !failed {
			t.Fatal("missing error")
		}
	}
}
func TestMessageIDContinuity(t *testing.T) {
	input := `data: {"v":{"response":{"message_id":42,"fragments":[{"type":"RESPONSE","content":"OK"}]}}}

data: [DONE]

`
	for chunk := range NewStreamParser(false, "s").ParseStream(context.Background(), strings.NewReader(input)) {
		if chunk.Done && chunk.Upstream.MessageID != 42 {
			t.Fatal("message id lost")
		}
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}
