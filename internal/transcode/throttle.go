package transcode

import "time"

// Throttling (DESIGN §8.3). ffmpeg encodes far faster than real time (17×
// for a GPU-decoded 1080p HEVC source), and left alone it runs to the end
// of the film: CPU and GPU busy for nothing, and the whole remote file
// pulled through the addon's proxy. Like Jellyfin's throttler, a session is
// paused (SIGSTOP) once it is throttleAhead past the last segment the
// player asked for, and resumed (SIGCONT) when the player is within
// throttleResume of it. A server may drop a paused remote input; ffmpeg's
// -reconnect flags reopen it at the same offset on resume.
var (
	throttleAhead  = 120 * time.Second
	throttleResume = 60 * time.Second
	throttleEvery  = time.Second // how often Manager.Run re-checks
)

// Requested records that the player asked for segment n, and resumes a
// paused ffmpeg at once when that brings it within reach.
func (s *Session) Requested(n int) {
	s.mu.Lock()
	s.requested = n
	s.mu.Unlock()
	s.throttle()
}

// Paused reports whether ffmpeg is held by the throttle.
func (s *Session) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// throttle pauses or resumes ffmpeg by how far it has written past the last
// segment the player asked for.
func (s *Session) throttle() {
	select {
	case <-s.done: // exited: nothing to hold
		return
	default:
	}
	written := s.Next() - 1
	seg := time.Duration(s.Opts.segmentSeconds()) * time.Second
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return
	}
	ahead := time.Duration(written-s.requested) * seg
	switch {
	case !s.paused && ahead >= throttleAhead:
		if pauseProcess(s.proc) == nil {
			s.paused = true
		}
	case s.paused && ahead < throttleResume:
		if resumeProcess(s.proc) == nil {
			s.paused = false
		}
	}
}

// throttleAll applies the throttle to every session.
func (m *Manager) throttleAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.throttle()
	}
}
