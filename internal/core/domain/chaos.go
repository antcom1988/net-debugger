package domain

// ChaosRule defines a pattern-based rule for injecting failures or delays.
type ChaosRule struct {
	ID                 string  `json:"id"`
	Service            string  `json:"service"`              // e.g. "postgres", "mariadb", "redis", or "" (any)
	MatchType          string  `json:"match_type"`           // "substring", "regex", "exact"
	Pattern            string  `json:"pattern"`              // Target query/table/command pattern
	Action             string  `json:"action"`               // "delay", "error", "close", "empty", "throttle"
	DelayRequestMs     int     `json:"delay_request_ms"`     // Delay before forwarding a request to the backend
	DelayResponseMs    int     `json:"delay_response_ms"`    // Delay before forwarding a backend response to the client
	APIStatusCode      int     `json:"api_status_code"`      // API custom response status
	APIResponseBody    string  `json:"api_response_body"`    // API custom response body
	APIHeaderName      string  `json:"api_header_name"`      // API response header name
	APIHeaderValue     string  `json:"api_header_value"`     // API response header value
	ErrorMsg           string  `json:"error_msg"`            // Used if Action = "error"
	ThrottleBytes      int     `json:"throttle_bytes"`       // Chunk size in bytes for Action = "throttle"
	ThrottleIntervalMs int     `json:"throttle_interval_ms"` // Interval in ms for Action = "throttle"
	Probability        float64 `json:"probability"`          // Chance of trigger (0.0 to 1.0)
	Enabled            bool    `json:"enabled"`
}
