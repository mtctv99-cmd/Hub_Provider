package pow

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// Vector kiểm thử từ DeepSeek WASM.
func TestDeepSeekHashV1(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "e594808bc5b7151ac160c6d39a02e0a8e261ed588578403099e3561dc40c26b3"},
		{"testsalt_1700000000_42", "d4a2ea58c89e40887c933484868380c6f803eaa8dc53a3b9df8e431b921a4f09"},
		{"testsalt_1700000000_100000", "abea2f35796b65486e9be1b36f7878c66cab021e96faa473fdf4decd31f9ba30"},
		{"abc123salt_1700000000_12345", "74b3b7452745b70e85eb32ee7f0a9ec0381d42dd5137b695da915e104fc390e1"},
	} {
		h := DeepSeekHashV1([]byte(tc.in))
		got := hex.EncodeToString(h[:])
		if got != tc.want {
			t.Errorf("hash(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSolvePow(t *testing.T) {
	for _, tc := range []struct {
		salt   string
		expire int64
		answer int64
		diff   int64
	}{
		{"testsalt", 1700000000, 42, 1000},
		{"testsalt", 1700000000, 500, 2000},
		{"abc123salt", 1700000000, 12345, 20000},
	} {
		h := DeepSeekHashV1([]byte(BuildPrefix(tc.salt, tc.expire) + strconv.FormatInt(tc.answer, 10)))
		got, err := SolvePow(context.Background(), hex.EncodeToString(h[:]), tc.salt, tc.expire, tc.diff)
		if err != nil || got != tc.answer {
			t.Errorf("salt=%q answer=%d: got=%d err=%v", tc.salt, tc.answer, got, err)
		}
	}
}

func TestSolvePowNoSolution(t *testing.T) {
	h := DeepSeekHashV1([]byte("salt_1700000000_9999"))
	_, err := SolvePow(context.Background(), hex.EncodeToString(h[:]), "salt", 1700000000, 100)
	if err == nil {
		t.Fatal("expected error when answer exceeds difficulty")
	}
}

func TestSolvePowInvalidHex(t *testing.T) {
	_, err := SolvePow(context.Background(), "invalid-hex", "salt", 1700000000, 100)
	if err == nil {
		t.Fatal("expected error on invalid challenge hex")
	}
}

func TestSolvePowContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel ngay lập tức

	h := DeepSeekHashV1([]byte("salt_1700000000_50000"))
	_, err := SolvePow(ctx, hex.EncodeToString(h[:]), "salt", 1700000000, 100000)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
}

func TestParseChallengeDirect(t *testing.T) {
	rawJSON := []byte(`{
		"algorithm": "DeepSeekHashV1",
		"challenge": "e594808bc5b7151ac160c6d39a02e0a8e261ed588578403099e3561dc40c26b3",
		"salt": "test_salt",
		"expire_at": 1720000000,
		"difficulty": 1000,
		"signature": "sig_xyz",
		"target_path": "/api/v0/chat/completion"
	}`)

	c, err := ParseChallenge(rawJSON)
	if err != nil {
		t.Fatalf("ParseChallenge failed: %v", err)
	}
	if c.Algorithm != "DeepSeekHashV1" || c.Salt != "test_salt" || c.Difficulty != 1000 {
		t.Errorf("unexpected challenge parsed: %+v", c)
	}
}

func TestParseChallengeNestedEnvelope(t *testing.T) {
	nestedJSON := []byte(`{
		"code": 0,
		"msg": "",
		"data": {
			"biz_data": {
				"challenge": {
					"algorithm": "DeepSeekHashV1",
					"challenge": "d4a2ea58c89e40887c933484868380c6f803eaa8dc53a3b9df8e431b921a4f09",
					"salt": "nested_salt",
					"expire_at": 1730000000,
					"difficulty": 144000,
					"signature": "sig_nested",
					"target_path": "/api/v0/chat/completion"
				}
			}
		}
	}`)

	c, err := ParseChallenge(nestedJSON)
	if err != nil {
		t.Fatalf("ParseChallenge nested failed: %v", err)
	}
	if c.Algorithm != "DeepSeekHashV1" || c.Salt != "nested_salt" || c.Difficulty != 144000 {
		t.Errorf("unexpected nested challenge parsed: %+v", c)
	}
}

func TestParseChallengeInvalid(t *testing.T) {
	_, err := ParseChallenge([]byte(`{}`))
	if err == nil {
		t.Fatal("expected error on empty object")
	}

	_, err = ParseChallenge([]byte(``))
	if err == nil {
		t.Fatal("expected error on empty byte slice")
	}
}

func TestBuildPowHeader(t *testing.T) {
	c := &Challenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "chal_hex",
		Salt:       "my_salt",
		ExpireAt:   1700000000,
		Difficulty: 10000,
		Signature:  "sig_abc",
		TargetPath: "/chat",
	}
	hdr, err := BuildPowHeader(c, 123)
	if err != nil {
		t.Fatalf("BuildPowHeader failed: %v", err)
	}

	dec, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(dec, &m); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	if m["algorithm"] != "DeepSeekHashV1" || m["answer"].(float64) != 123 {
		t.Errorf("unexpected payload: %v", m)
	}
	if _, ok := m["difficulty"]; ok {
		t.Errorf("header payload should not include difficulty: %v", m)
	}
	if _, ok := m["expire_at"]; ok {
		t.Errorf("header payload should not include expire_at: %v", m)
	}
}

func TestBuildPowHeaderFromMap(t *testing.T) {
	m := map[string]any{
		"algorithm":   "DeepSeekHashV1",
		"challenge":   "chal_hex",
		"salt":        "my_salt",
		"signature":   "sig_map",
		"target_path": "/chat",
	}
	hdr, err := BuildPowHeaderFromMap(m, 456)
	if err != nil {
		t.Fatalf("BuildPowHeaderFromMap failed: %v", err)
	}
	dec, _ := base64.StdEncoding.DecodeString(hdr)
	var res map[string]any
	_ = json.Unmarshal(dec, &res)
	if res["answer"].(float64) != 456 || res["signature"] != "sig_map" {
		t.Errorf("unexpected map header payload: %v", res)
	}
}

func TestSolveAndBuildHeader(t *testing.T) {
	t0 := DeepSeekHashV1([]byte("salt_1712345678_777"))
	header, err := SolveAndBuildHeader(context.Background(), &Challenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  hex.EncodeToString(t0[:]),
		Salt:       "salt",
		ExpireAt:   1712345678,
		Difficulty: 2000,
		Signature:  "sig",
		TargetPath: "/api/v0/chat/completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(header)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if int64(m["answer"].(float64)) != 777 {
		t.Errorf("answer = %v, want 777", m["answer"])
	}
}

func TestSolveAndBuildHeaderFromJSON(t *testing.T) {
	t0 := DeepSeekHashV1([]byte("jsonsalt_1719999999_888"))
	payload := map[string]any{
		"algorithm":   "DeepSeekHashV1",
		"challenge":   hex.EncodeToString(t0[:]),
		"salt":        "jsonsalt",
		"expire_at":   1719999999,
		"difficulty":  2000,
		"signature":   "sig_json",
		"target_path": "/api/v0/chat/completion",
	}
	b, _ := json.Marshal(payload)

	hdr, err := SolveAndBuildHeaderFromJSON(context.Background(), b)
	if err != nil {
		t.Fatalf("SolveAndBuildHeaderFromJSON error: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(hdr)
	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	if int64(res["answer"].(float64)) != 888 {
		t.Errorf("expected answer 888, got %v", res["answer"])
	}
}

func BenchmarkHash(b *testing.B) {
	d := []byte("realisticsalt_1712345678_12345")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DeepSeekHashV1(d)
	}
}

func BenchmarkSolve(b *testing.B) {
	h := DeepSeekHashV1([]byte("realisticsalt_1712345678_72000"))
	ch := hex.EncodeToString(h[:])
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = SolvePow(context.Background(), ch, "realisticsalt", 1712345678, 144000)
	}
}

// ponytail: single self-check ensuring end-to-end sanity in non-test runtimes if imported
func init() {
	h := DeepSeekHashV1([]byte(""))
	if hex.EncodeToString(h[:]) != "e594808bc5b7151ac160c6d39a02e0a8e261ed588578403099e3561dc40c26b3" {
		panic("pow: self-check failed for empty string hash")
	}
	_ = time.Second
}
