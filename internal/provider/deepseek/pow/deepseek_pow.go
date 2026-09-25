package pow

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

// HeaderName là tên HTTP header của DeepSeek PoW response.
const HeaderName = "x-ds-pow-response"

// DefaultDifficulty độ khó mặc định nếu challenge không cung cấp.
const DefaultDifficulty int64 = 144000

// Challenge chứa thông số challenge từ DeepSeek /api/v0/chat/create_pow_challenge.
type Challenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	ExpireAt   int64  `json:"expire_at"`
	Difficulty int64  `json:"difficulty"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// ParseChallenge phân tích JSON (object Challenge trực tiếp hoặc envelope lồng nhau từ DeepSeek).
func ParseChallenge(data []byte) (*Challenge, error) {
	if len(data) == 0 {
		return nil, errors.New("pow: empty challenge JSON")
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	m := raw
	if d, ok := m["data"].(map[string]any); ok {
		m = d
	}
	if b, ok := m["biz_data"].(map[string]any); ok {
		m = b
	}
	if c, ok := m["challenge"].(map[string]any); ok {
		m = c
	}

	getString := func(k string) string {
		if s, ok := m[k].(string); ok {
			return s
		}
		return ""
	}

	getInt64 := func(k string, def int64) int64 {
		v, ok := m[k]
		if !ok || v == nil {
			return def
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case float32:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		case string:
			if parsed, err := strconv.ParseInt(n, 10, 64); err == nil {
				return parsed
			}
		}
		return def
	}

	ch := &Challenge{
		Algorithm:  getString("algorithm"),
		Challenge:  getString("challenge"),
		Salt:       getString("salt"),
		ExpireAt:   getInt64("expire_at", 0),
		Difficulty: getInt64("difficulty", DefaultDifficulty),
		Signature:  getString("signature"),
		TargetPath: getString("target_path"),
	}

	if ch.Algorithm == "" {
		return nil, errors.New("pow: missing algorithm in challenge")
	}
	if ch.Challenge == "" {
		return nil, errors.New("pow: missing challenge string")
	}
	return ch, nil
}

// BuildPrefix: "<salt>_<expire_at>_"
func BuildPrefix(salt string, expireAt int64) string {
	return salt + "_" + strconv.FormatInt(expireAt, 10) + "_"
}

// SolvePow tìm nonce ∈ [0, difficulty) sao cho DeepSeekHashV1(prefix + str(nonce)) == challenge.
// prefix được hấp thu trước vào Keccak state, vòng lặp tìm nonce zero-allocation.
// ponytail: single-threaded search; add multi-goroutine sharding when difficulty exceeds 10^6.
func SolvePow(ctx context.Context, challengeHex, salt string, expireAt, difficulty int64) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(challengeHex) != 64 {
		return 0, errors.New("pow: challenge must be 64 hex chars")
	}
	target, err := hex.DecodeString(challengeHex)
	if err != nil {
		return 0, err
	}
	var ta [32]byte
	copy(ta[:], target)
	t0 := binary.LittleEndian.Uint64(ta[0:])
	t1 := binary.LittleEndian.Uint64(ta[8:])
	t2 := binary.LittleEndian.Uint64(ta[16:])
	t3 := binary.LittleEndian.Uint64(ta[24:])

	prefix := []byte(BuildPrefix(salt, expireAt))
	const rate = 136
	var baseState [25]uint64
	off := 0
	for off+rate <= len(prefix) {
		for i := 0; i < rate/8; i++ {
			baseState[i] ^= binary.LittleEndian.Uint64(prefix[off+i*8:])
		}
		keccakF23(&baseState)
		off += rate
	}
	tailLen := len(prefix) - off
	var tail [rate]byte
	copy(tail[:], prefix[off:])

	var numBuf [20]byte
	for n := int64(0); n < difficulty; n++ {
		// Kiểm tra context cancellation định kỳ để tránh tốn CPU vô ích
		if n&0x3FF == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}

		v := uint64(n)
		pos := 20
		if v == 0 {
			pos--
			numBuf[pos] = '0'
		} else {
			for v > 0 {
				pos--
				numBuf[pos] = byte('0' + v%10)
				v /= 10
			}
		}
		numLen := 20 - pos
		s := baseState
		totalTail := tailLen + numLen
		if totalTail < rate {
			var buf [rate]byte
			copy(buf[:tailLen], tail[:tailLen])
			copy(buf[tailLen:totalTail], numBuf[pos:])
			buf[totalTail] = 0x06
			buf[rate-1] |= 0x80
			for i := 0; i < rate/8; i++ {
				s[i] ^= binary.LittleEndian.Uint64(buf[i*8:])
			}
			keccakF23(&s)
		} else {
			var buf [rate]byte
			copy(buf[:tailLen], tail[:tailLen])
			copy(buf[tailLen:rate], numBuf[pos:pos+(rate-tailLen)])
			for i := 0; i < rate/8; i++ {
				s[i] ^= binary.LittleEndian.Uint64(buf[i*8:])
			}
			keccakF23(&s)
			var buf2 [rate]byte
			rem := totalTail - rate
			copy(buf2[:rem], numBuf[pos+(rate-tailLen):pos+(rate-tailLen)+rem])
			buf2[rem] = 0x06
			buf2[rate-1] |= 0x80
			for i := 0; i < rate/8; i++ {
				s[i] ^= binary.LittleEndian.Uint64(buf2[i*8:])
			}
			keccakF23(&s)
		}
		if s[0] == t0 && s[1] == t1 && s[2] == t2 && s[3] == t3 {
			return n, nil
		}
	}
	return 0, errors.New("pow: no solution within difficulty")
}

// BuildPowHeader tuần tự hóa {algorithm,challenge,salt,answer,signature,target_path} thành base64(JSON).
// Header này không chứa difficulty / expire_at.
func BuildPowHeader(c *Challenge, answer int64) (string, error) {
	if c == nil {
		return "", errors.New("pow: nil challenge")
	}
	b, err := json.Marshal(map[string]any{
		"algorithm":   c.Algorithm,
		"challenge":   c.Challenge,
		"salt":        c.Salt,
		"answer":      answer,
		"signature":   c.Signature,
		"target_path": c.TargetPath,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// BuildPowHeaderFromMap hỗ trợ tuần tự hóa từ challenge map.
func BuildPowHeaderFromMap(challenge map[string]any, answer int64) (string, error) {
	payload := map[string]any{
		"algorithm":   challenge["algorithm"],
		"challenge":   challenge["challenge"],
		"salt":        challenge["salt"],
		"answer":      answer,
		"signature":   challenge["signature"],
		"target_path": challenge["target_path"],
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// SolveAndBuildHeader giải PoW và trả về header x-ds-pow-response từ *Challenge.
func SolveAndBuildHeader(ctx context.Context, c *Challenge) (string, error) {
	if c == nil {
		return "", errors.New("pow: nil challenge")
	}
	if c.Algorithm != "DeepSeekHashV1" {
		return "", errors.New("pow: unsupported algorithm: " + c.Algorithm)
	}
	d := c.Difficulty
	if d <= 0 {
		d = DefaultDifficulty
	}
	answer, err := SolvePow(ctx, c.Challenge, c.Salt, c.ExpireAt, d)
	if err != nil {
		return "", err
	}
	return BuildPowHeader(c, answer)
}

// SolveAndBuildHeaderFromJSON phân tích JSON challenge và trả về header x-ds-pow-response.
func SolveAndBuildHeaderFromJSON(ctx context.Context, challengeJSON []byte) (string, error) {
	c, err := ParseChallenge(challengeJSON)
	if err != nil {
		return "", err
	}
	return SolveAndBuildHeader(ctx, c)
}
