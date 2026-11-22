package realtime

import "time"

// Options are the timings and limits of the realtime layer. Production uses DefaultOptions; tests shorten them.
type Options struct {
	// AuthTimeout: a connection that has not sent auth by then is closed (4001).
	AuthTimeout time.Duration
	// HeartbeatInterval is what clients are told to ping at. Cloudflare drops idle connections after about 100s.
	HeartbeatInterval time.Duration
	// IdleTimeout: a connection that sent no frame for this long is closed (4008).
	IdleTimeout time.Duration
	// PresenceTTL: a connection counts as online this long after its last ping, so a crashed pod's users go offline.
	PresenceTTL time.Duration
	// TypingExpiry: typing ends by itself when not refreshed. TypingMinInterval: at most one typing frame per channel
	// in this time, the rest is dropped.
	TypingExpiry      time.Duration
	TypingMinInterval time.Duration
	// SendBuffer is how many events may wait for a slow client before it is closed (1013).
	SendBuffer   int
	WriteTimeout time.Duration
	// MaxSessionsPerUser caps the connections of one user over all pods (4029 beyond it).
	MaxSessionsPerUser int
	// RequestsPerSecond caps the frames of one connection.
	RequestsPerSecond int
	// MaxFrameBytes is the largest frame accepted (the library closes with 1009 beyond it).
	MaxFrameBytes int64
	// HubBeatInterval and HubBeatTimeout: each pod publishes a beat to itself; when none arrives within the timeout
	// the Redis subscription is considered dead (a half-open connection) and is rebuilt.
	HubBeatInterval time.Duration
	HubBeatTimeout  time.Duration
}

func DefaultOptions() Options {
	return Options{
		AuthTimeout:        10 * time.Second,
		HeartbeatInterval:  25 * time.Second,
		IdleTimeout:        60 * time.Second,
		PresenceTTL:        70 * time.Second,
		TypingExpiry:       6 * time.Second,
		TypingMinInterval:  time.Second,
		SendBuffer:         256,
		WriteTimeout:       10 * time.Second,
		MaxSessionsPerUser: 10,
		RequestsPerSecond:  30,
		MaxFrameBytes:      64 << 10,
		HubBeatInterval:    10 * time.Second,
		HubBeatTimeout:     30 * time.Second,
	}
}
