package config

import (
	"crypto/tls"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server struct {
		Port int    `toml:"port"`
		Host string `toml:"host"`

		ProductionMode bool `toml:"production_mode"`
		SPAMode        bool `toml:"spa_mode"`

		MetricsEnabled   bool          `toml:"metrics_enabled"`
		CachingEnabled   bool          `toml:"caching_enabled"`
		CoalescerEnabled bool          `toml:"coalescer_enabled"`
		ReadTimeout      time.Duration `toml:"read_timeout"`
		WriteTimeout     time.Duration `toml:"write_timeout"`
		IdleTimeout      time.Duration `toml:"idle_timeout"`
		MaxHeaderBytes   int           `toml:"max_header_bytes"`
		EnableHTTP2      bool          `toml:"enable_http2"`
		TLSConfig        *tls.Config   `toml:"tls"`
	} `toml:"server"`

	Cache struct {
		MaxSize           int           `toml:"max_size"`
		DefaultExpiration time.Duration `toml:"default_expiration"`
	} `toml:"cache"`

	Metrics struct {
		CollectionInterval time.Duration `toml:"collection_interval"`
		RetentionPeriod    time.Duration `toml:"retention_period"`
	} `toml:"metrics"`

	Logging struct {
		Level string `toml:"level"`
		File  string `toml:"file"`
	} `toml:"logging"`

	Directories struct {
		Web       string `toml:"web"`
		Content   string `toml:"content"`
		Static    string `toml:"static"`
		Dist      string `toml:"dist"`
		Meta      string `toml:"meta"`
		Core      string `toml:"core"`
		Templates string `toml:"templates"`
	} `toml:"directories"`

	URLPrefixes struct {
		SPA    string `toml:"spa"`
		Static string `toml:"static"`
		Core   string `toml:"core"`
	} `toml:"url_prefixes"`

	Templates struct {
		Main string `toml:"main"`
	} `toml:"templates"`

	Build struct {
		IgnoreFile string `toml:"ignore_file"`
	} `toml:"build"`

	// Sites lists every content site under web/<name>/. Each maps one or
	// more Host headers to a separate manifest. The "*" host marks the
	// fallback site used when the request Host doesn't match any other
	// site. Studio is *not* in this list — it's a module-managed site
	// with its own builder (see cmd/build/sites.go).
	Sites []Site `toml:"site"`
}

type Site struct {
	Name  string   `toml:"name"`
	Hosts []string `toml:"hosts"`
}

func LoadConfig(path string) (Config, error) {
	var cfg Config

	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}
