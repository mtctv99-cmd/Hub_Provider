package admin

import "testing"

func TestInferProvider(t *testing.T) {
	cases := map[string]string{
		"gemini-3.8-flash":   "gemini_web",
		"gpt-5.6":            "copilot_ws",
		"copilot-chat":       "copilot_ws",
		"deepseek-chat":      "deepseek_web",
		"tiktok:vi_male":     "tiktok_api",
		"piper:ngochuyennew": "piper_local",
		"gemma4-e4b":         "google_colab",
		"muse-chat":          "meta_web",
		"unknown-model":      "",
	}
	for model, want := range cases {
		if got := inferProvider(model); got != want {
			t.Fatalf("inferProvider(%q)=%q, want %q", model, got, want)
		}
	}
}
