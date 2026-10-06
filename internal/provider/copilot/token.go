package copilot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var chathubPath = regexp.MustCompile(`^/m365Copilot/Chathub/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}@[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateTokenURL checks the observed Copilot endpoint and unsigned token freshness.
// It does not verify the JWT signature or prove that Microsoft accepts the token.
func ValidateTokenURL(tokenURL string, minSeconds int64) (time.Time, error) {
	if len(tokenURL) > 16384 || len(tokenURL) == 0 || strings.TrimSpace(tokenURL) != tokenURL || strings.ContainsAny(tokenURL, "\r\n\t") {
		return time.Time{}, errors.New("invalid Copilot token URL")
	}
	u, err := url.Parse(tokenURL)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.Port() != "" || u.RawPath != "" {
		return time.Time{}, errors.New("invalid Copilot endpoint")
	}
	if u.Hostname() != "substrate.office.com" && u.Hostname() != "substrate.svc.cloud.microsoft" {
		return time.Time{}, errors.New("untrusted Copilot endpoint")
	}
	if !chathubPath.MatchString(u.Path) {
		return time.Time{}, errors.New("invalid Copilot path")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values["access_token"]) != 1 || len(values["access_token"][0]) > 12000 {
		return time.Time{}, errors.New("invalid Copilot token")
	}
	parts := strings.Split(values["access_token"][0], ".")
	if len(parts) != 3 || len(parts[0]) > 2048 || len(parts[1]) > 8192 || len(parts[2]) > 4096 {
		return time.Time{}, errors.New("invalid Copilot token")
	}
	header, err := decodeBase64URL(parts[0])
	if err != nil {
		return time.Time{}, errors.New("invalid Copilot token header")
	}
	var algorithm struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(header, &algorithm) != nil || algorithm.Alg == "" || strings.EqualFold(algorithm.Alg, "none") {
		return time.Time{}, errors.New("invalid Copilot token algorithm")
	}
	payload, err := decodeBase64URL(parts[1])
	if err != nil || parts[2] == "" {
		return time.Time{}, errors.New("invalid Copilot token payload")
	}
	var claims struct {
		Exp int64 `json:"exp"`
		Nbf int64 `json:"nbf"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= time.Now().Unix()+minSeconds || claims.Nbf > time.Now().Unix()+60 {
		return time.Time{}, errors.New("Copilot token expired or not yet valid")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

// TokenExpiresSoon reports whether the token expires within lead time.
// Unparseable tokens return true so callers refresh conservatively.
func TokenExpiresSoon(tokenURL string, lead time.Duration) bool {
	exp, err := ValidateTokenURL(tokenURL, 0)
	if err != nil {
		return true
	}
	return time.Until(exp) <= lead
}

func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

var copilotEmail = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)

// ValidateCapturedToken binds the observed Chathub path to the JWT user and
// tenant. Claims are unverified metadata, not proof of Microsoft authentication.
func ValidateCapturedToken(tokenURL string, minSeconds int64) (time.Time, string, error) {
	expiry, err := ValidateTokenURL(tokenURL, minSeconds)
	if err != nil {
		return time.Time{}, "", err
	}
	u, _ := url.Parse(tokenURL)
	parts := strings.Split(u.Query().Get("access_token"), ".")
	data, err := decodeBase64URL(parts[1])
	if err != nil {
		return time.Time{}, "", errors.New("invalid token claims")
	}
	var claims struct {
		OID               string `json:"oid"`
		TID               string `json:"tid"`
		UPN               string `json:"upn"`
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		UniqueName        string `json:"unique_name"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return time.Time{}, "", errors.New("invalid token claims")
	}
	pathID := strings.TrimPrefix(u.Path, "/m365Copilot/Chathub/")
	ids := strings.Split(pathID, "@")
	if len(ids) != 2 || !strings.EqualFold(ids[0], claims.OID) || !strings.EqualFold(ids[1], claims.TID) {
		return time.Time{}, "", errors.New("Copilot user does not match token")
	}
	email := strings.TrimSpace(claims.PreferredUsername)
	if email == "" {
		email = strings.TrimSpace(claims.UPN)
	}
	if email == "" {
		email = strings.TrimSpace(claims.Email)
	}
	if email == "" {
		email = strings.TrimSpace(claims.UniqueName)
	}
	if !copilotEmail.MatchString(email) || len(email) > 320 {
		return time.Time{}, "", errors.New("Copilot token has no user email")
	}
	return expiry, email, nil
}
