package domain

import "time"

// LogEntry represents an intercepted query or command log entry.
type LogEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	ThirdParty string    `json:"third_party"` // "redis", "postgres", or "mariadb"
	ClientIP   string    `json:"client_ip"`
	Query      string    `json:"query"`
	Response   string    `json:"response"`
}
