package identity

import "time"

type IdentityStatus string

const (
	IdentityHealthy      IdentityStatus = "HEALTHY"
	IdentityPartial      IdentityStatus = "PARTIAL"
	IdentityDegraded     IdentityStatus = "DEGRADED"
	IdentityDisabled     IdentityStatus = "DISABLED"
	IdentityUnconfigured IdentityStatus = "UNCONFIGURED"
)

type PoolRole string

const (
	PoolAuto    PoolRole = "AUTO"
	PoolSession PoolRole = "SESSION"
	PoolWorker  PoolRole = "WORKER"
)

type CredentialStatus string

const (
	StatusHealthy      CredentialStatus = "HEALTHY"
	StatusDegraded     CredentialStatus = "DEGRADED"
	StatusRateLimited  CredentialStatus = "RATE_LIMITED"
	StatusCooldown     CredentialStatus = "COOLDOWN"
	StatusExpired      CredentialStatus = "EXPIRED"
	StatusAuthRequired CredentialStatus = "AUTH_REQUIRED"
	StatusDisabled     CredentialStatus = "DISABLED"
)

type Identity struct {
	ID            string         `json:"id"`
	ProviderGroup string         `json:"provider_group"`
	Email         string         `json:"email"`
	Priority      int            `json:"priority"`
	GlobalStatus  IdentityStatus `json:"global_status"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type Credential struct {
	ID                string           `json:"id"`
	IdentityID        string           `json:"identity_id"`
	Domain            string           `json:"domain"`
	Status            CredentialStatus `json:"status"`
	MaxConcurrency    int              `json:"max_concurrency"`
	ActiveLeases      int32            `json:"active_leases"`
	CooldownUntil     *time.Time       `json:"cooldown_until,omitempty"`
	ConsecutiveErrors int              `json:"consecutive_errors"`
	LastUsedAt        *time.Time       `json:"last_used_at,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	SourceRef         string           `json:"source_ref,omitempty"`
	Fingerprint       string           `json:"fingerprint,omitempty"`
	PoolRole          PoolRole         `json:"pool_role"`
	LastErrorKind     string           `json:"last_error_kind,omitempty"`
}

type RuntimeCapacity struct {
	CredentialID   string           `json:"credential_id"`
	Domain         string           `json:"domain"`
	Status         CredentialStatus `json:"status"`
	MaxConcurrency int              `json:"max_concurrency"`
	ActiveLeases   int32            `json:"active_leases"`
	CooldownUntil  time.Time        `json:"cooldown_until,omitempty"`
	BackoffStreak  int              `json:"backoff_streak"`
}

type IdentityDetail struct {
	Identity    Identity      `json:"identity"`
	Credentials []*Credential `json:"credentials"`
}
