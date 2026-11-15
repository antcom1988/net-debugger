package domain

// ServiceConfig defines the configuration for a specific database/service proxy.
type ServiceConfig struct {
	Type        string `json:"type"` // "redis", "postgres", "mariadb", "api", "iso8583"
	ListenPort  int    `json:"listen_port"`
	BackendAddr string `json:"backend_addr"`
	LabelColor  string `json:"label_color"`
	Enabled     bool   `json:"enabled"`
}

// WebConfig defines the configuration for the HTTP web dashboard server.
type WebConfig struct {
	Port    int  `json:"port"`
	Enabled bool `json:"enabled"`
}

// AppConfig defines the global application configurations.
type AppConfig struct {
	RetentionDays int                      `json:"retention_days"`
	LogDir        string                   `json:"log_dir"`
	Services      map[string]ServiceConfig `json:"services"`
	Web           WebConfig                `json:"web"`
}
