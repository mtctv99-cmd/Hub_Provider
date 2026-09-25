package jobs

import (
	"context"
	"log"
	"time"

	"sever_provider/internal/filestore"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/session"
)

// Supervisor periodically performs background maintenance:
// - resets stalled/abandoned processing jobs back to pending
// - cleans up old completed/failed job records
// - purges expired temporary media files from filestore
// - sweeps expired sticky sessions
type Supervisor struct {
	Jobs     *jobstore.Store
	Files    *filestore.Store
	Sessions *session.Manager
	Interval time.Duration
}

func (s *Supervisor) Start(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.RunOnce(ctx)
			}
		}
	}()
}

func (s *Supervisor) RunOnce(ctx context.Context) {
	if s.Jobs != nil {
		// Reset jobs that have been locked for more than 10 minutes without completion
		if n, err := s.Jobs.ResetStale(ctx, 10*time.Minute); err == nil && n > 0 {
			log.Printf("[SUPERVISOR] reset %d stale processing jobs", n)
		}
		// Cleanup completed/failed jobs older than 7 days
		if n, err := s.Jobs.CleanupOld(ctx, 7*24*time.Hour); err == nil && n > 0 {
			log.Printf("[SUPERVISOR] cleaned up %d old job records", n)
		}
	}
	if s.Files != nil {
		// Cleanup media files older than 24 hours
		if removed, err := s.Files.Cleanup(24 * time.Hour); err == nil && len(removed) > 0 {
			log.Printf("[SUPERVISOR] cleaned up %d expired media files", len(removed))
		}
	}
	if s.Sessions != nil {
		// Sweep expired sticky sessions
		if n := s.Sessions.Sweep(); n > 0 {
			log.Printf("[SUPERVISOR] swept %d expired sticky sessions", n)
		}
	}
}
