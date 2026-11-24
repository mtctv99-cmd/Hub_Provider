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
	// StaleAfter is the stale window for chat/TTS/LLM jobs. Zero uses 10m.
	StaleAfter time.Duration
	// VideoTimeout mirrors Runner.VideoTimeout. Video jobs are only reset
	// after VideoTimeout+VideoStaleBuffer (with a 60m floor), so a healthy
	// 45m generation is never requeued while it is still running.
	VideoTimeout time.Duration
	// VideoStaleAfter overrides the computed video stale window when > 0.
	VideoStaleAfter time.Duration
}

const (
	defaultStaleAfter      = 10 * time.Minute
	defaultVideoStaleAfter = 60 * time.Minute
)

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

// staleWindows returns the non-video and video stale thresholds. They are split
// per kind because a video generation legitimately runs for up to
// VideoTimeout, while a chat job stuck for 10m is abandoned.
func (s *Supervisor) staleWindows() (time.Duration, time.Duration) {
	nonVideo := s.StaleAfter
	if nonVideo <= 0 {
		nonVideo = defaultStaleAfter
	}
	video := s.VideoStaleAfter
	if video <= 0 {
		videoTimeout := s.VideoTimeout
		if videoTimeout <= 0 {
			videoTimeout = DefaultVideoTimeout
		}
		video = videoTimeout + VideoStaleBuffer
		if video < defaultVideoStaleAfter {
			video = defaultVideoStaleAfter
		}
	}
	return nonVideo, video
}

func (s *Supervisor) RunOnce(ctx context.Context) {
	if s.Jobs != nil {
		nonVideo, video := s.staleWindows()
		// Abandoned chat/TTS jobs keep the short window; video jobs are reset
		// only once their own generator timeout has passed.
		if n, err := s.Jobs.ResetStaleExceptType(ctx, nonVideo, "video"); err == nil && n > 0 {
			log.Printf("[SUPERVISOR] reset %d stale processing jobs", n)
		}
		if n, err := s.Jobs.ResetStaleType(ctx, video, "video"); err == nil && n > 0 {
			log.Printf("[SUPERVISOR] reset %d stale video processing jobs (window=%s)", n, video)
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
