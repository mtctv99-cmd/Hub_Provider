package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidAPIKey     = errors.New("invalid API key")
	ErrExpiredAPIKey     = errors.New("API key expired")
	ErrInsufficientScope = errors.New("insufficient API key scope")
	ErrRateLimit         = errors.New("API key rate limit exceeded")
	ErrInvalidKeyOptions = errors.New("invalid API key options")
)

var allowedScopes = map[string]bool{"all": true, "chat": true, "audio_speech": true, "images": true, "models": true, "jobs": true, "video": true}

type APIKey struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Active       bool       `json:"active"`
	Scopes       []string   `json:"scopes"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	RateLimitRPM int        `json:"rate_limit_rpm"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

type CreateKeySpec struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days"`
	RateLimitRPM  int      `json:"rate_limit_rpm"`
}

type APIKeys struct{ db *sql.DB }

func NewAPIKeys(db *sql.DB) *APIKeys { return &APIKeys{db: db} }

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-sp-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *APIKeys) Create(name string) (APIKey, string, error) {
	return s.CreateWithOptions(CreateKeySpec{Name: name})
}

func (s *APIKeys) CreateWithOptions(spec CreateKeySpec) (APIKey, string, error) {
	if s == nil || s.db == nil {
		return APIKey{}, "", errors.New("api key store unavailable")
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if len(spec.Name) == 0 || len(spec.Name) > 120 || spec.ExpiresInDays < 0 || spec.ExpiresInDays > 3650 || spec.RateLimitRPM < 0 || spec.RateLimitRPM > 100000 {
		return APIKey{}, "", ErrInvalidKeyOptions
	}
	if len(spec.Scopes) == 0 {
		spec.Scopes = []string{"all"}
	}
	seen := map[string]bool{}
	for _, scope := range spec.Scopes {
		if !allowedScopes[scope] || seen[scope] {
			return APIKey{}, "", ErrInvalidKeyOptions
		}
		seen[scope] = true
	}
	if len(spec.Scopes) > 1 && seen["all"] {
		return APIKey{}, "", ErrInvalidKeyOptions
	}
	plain, err := randomKey()
	if err != nil {
		return APIKey{}, "", err
	}
	seed, err := randomKey()
	if err != nil {
		return APIKey{}, "", err
	}
	now := time.Now().UTC()
	meta := APIKey{ID: hashKey(seed)[:24], Name: spec.Name, Active: true, Scopes: spec.Scopes, RateLimitRPM: spec.RateLimitRPM, CreatedAt: now}
	if spec.ExpiresInDays > 0 {
		exp := now.AddDate(0, 0, spec.ExpiresInDays)
		meta.ExpiresAt = &exp
	}
	_, err = s.db.Exec(`INSERT INTO api_keys(id,name,key_hash,active,scopes,expires_at,rate_limit_rpm) VALUES(?,?,?,1,?,?,?)`, meta.ID, meta.Name, hashKey(plain), strings.Join(meta.Scopes, ","), meta.ExpiresAt, meta.RateLimitRPM)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("create api key: %w", err)
	}
	return meta, plain, nil
}

func (s *APIKeys) List() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT id,name,active,scopes,expires_at,rate_limit_rpm,created_at,last_used_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	for rows.Next() {
		var k APIKey
		var active int
		var scopes string
		var expiry, last sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &active, &scopes, &expiry, &k.RateLimitRPM, &k.CreatedAt, &last); err != nil {
			return nil, err
		}
		k.Active = active != 0
		k.Scopes = strings.Split(scopes, ",")
		if expiry.Valid {
			k.ExpiresAt = &expiry.Time
		}
		if last.Valid {
			k.LastUsedAt = &last.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (s *APIKeys) Revoke(id string) error {
	r, err := s.db.Exec(`UPDATE api_keys SET active=0 WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *APIKeys) Activate(id string) error {
	r, err := s.db.Exec(`UPDATE api_keys SET active=1 WHERE id=? AND (expires_at IS NULL OR expires_at > ?)`, id, time.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		return ErrExpiredAPIKey
	}
	return nil
}
func (s *APIKeys) Delete(id string) error {
	r, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *APIKeys) HasActiveKeys() bool { return s.HasAnyKeys() }
func (s *APIKeys) HasAnyKeys() bool {
	if s == nil || s.db == nil {
		return false
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&count); err != nil {
		return true
	}
	return count > 0
}

func (s *APIKeys) Authenticate(candidate string) bool {
	_, err := s.AuthenticateWithScope(candidate, "")
	return err == nil
}

func (s *APIKeys) AuthenticateWithScope(candidate, scope string) (APIKey, error) {
	if s == nil || s.db == nil || strings.TrimSpace(candidate) == "" {
		return APIKey{}, ErrInvalidAPIKey
	}
	var key APIKey
	var active int
	var scopes string
	var expiry sql.NullTime
	err := s.db.QueryRow(`SELECT id,name,active,scopes,expires_at,rate_limit_rpm FROM api_keys WHERE key_hash=?`, hashKey(candidate)).Scan(&key.ID, &key.Name, &active, &scopes, &expiry, &key.RateLimitRPM)
	if err != nil {
		return APIKey{}, ErrInvalidAPIKey
	}
	if active == 0 {
		return APIKey{}, ErrInvalidAPIKey
	}
	if expiry.Valid {
		key.ExpiresAt = &expiry.Time
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now().UTC()) {
		return APIKey{}, ErrExpiredAPIKey
	}
	key.Scopes = strings.Split(scopes, ",")
	key.Active = true
	if scope != "" {
		allowed := false
		for _, permitted := range key.Scopes {
			permitted = strings.TrimSpace(permitted)
			if permitted == "all" || permitted == scope {
				allowed = true
				break
			}
			// Alternate-scope paths use "a|b", e.g. /v1/files accepts images or video.
			for _, alternative := range strings.Split(scope, "|") {
				if permitted == strings.TrimSpace(alternative) {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
		if !allowed {
			return APIKey{}, ErrInsufficientScope
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return APIKey{}, err
	}
	defer tx.Rollback()
	if key.RateLimitRPM > 0 {
		window := time.Now().UTC().Unix() / 60 * 60
		r, err := tx.Exec(`INSERT INTO api_key_rate_windows(key_id,window_start,request_count) VALUES(?,?,1)
			ON CONFLICT(key_id,window_start) DO UPDATE SET request_count=request_count+1 WHERE request_count < ?`, key.ID, window, key.RateLimitRPM)
		if err != nil {
			return APIKey{}, err
		}
		if n, err := r.RowsAffected(); err != nil {
			return APIKey{}, err
		} else if n == 0 {
			return APIKey{}, ErrRateLimit
		}
	}
	updated, err := tx.Exec(`UPDATE api_keys SET last_used_at=CURRENT_TIMESTAMP WHERE id=? AND active=1 AND (expires_at IS NULL OR expires_at > ?)`, key.ID, time.Now().UTC())
	if err != nil {
		return APIKey{}, err
	}
	if count, err := updated.RowsAffected(); err != nil {
		return APIKey{}, err
	} else if count == 0 {
		return APIKey{}, ErrInvalidAPIKey
	}
	if err := tx.Commit(); err != nil {
		return APIKey{}, err
	}
	return key, nil
}
