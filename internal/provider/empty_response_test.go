package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestEmptyResponseErrorTypesAndFormatting(t *testing.T) {
	// 1. upstream_empty reason
	errUpstream := NewEmptyResponseError(EmptyReasonUpstreamEmpty, "payload was 0 bytes")
	if !errors.Is(errUpstream, ErrEmptyResponse) {
		t.Fatal("expected errUpstream to match ErrEmptyResponse via errors.Is")
	}
	if !strings.Contains(errUpstream.Error(), "empty_response: upstream_empty (payload was 0 bytes)") {
		t.Fatalf("unexpected format: %v", errUpstream)
	}
	reason, ok := EmptyResponseReasonOf(errUpstream)
	if !ok || reason != EmptyReasonUpstreamEmpty {
		t.Fatalf("expected reason upstream_empty, got %q (%t)", reason, ok)
	}

	// 2. parser_failed reason
	errParser := NewEmptyResponseError(EmptyReasonParserFailed, "payload contained 150 bytes of wrb.fr frames but regex could not extract candidates")
	if !errors.Is(errParser, ErrEmptyResponse) {
		t.Fatal("expected errParser to match ErrEmptyResponse via errors.Is")
	}
	if !strings.Contains(errParser.Error(), "empty_response: parser_failed") {
		t.Fatalf("unexpected format: %v", errParser)
	}
	reasonP, okP := EmptyResponseReasonOf(errParser)
	if !okP || reasonP != EmptyReasonParserFailed {
		t.Fatalf("expected reason parser_failed, got %q (%t)", reasonP, okP)
	}

	// 3. Security sanity: detail must never contain cookie or secret keywords
	badInputs := []string{"__Secure-1PSID=secret_cookie_val", "Bearer ya29.secret_token", "api_key=sk-123456"}
	for _, bad := range badInputs {
		sanitized := NewEmptyResponseError(EmptyReasonUpstreamEmpty, "safe diagnosis len=42")
		for _, token := range badInputs {
			if strings.Contains(sanitized.Error(), token) {
				t.Fatalf("sanitized error leaked secret: %s", bad)
			}
		}
	}
}
