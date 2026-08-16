// Package config loads, merges and validates true-dns configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"truedns/internal/paths"
)

// Mode selects how DNS requests are routed.
type Mode string

const (
	// ModeSplit routes only domains matching the polluted list through DoH;
	// everything else goes to the system resolver. Least invasive.
	ModeSplit Mode = "split"
	// ModeFull routes every request through DoH upstreams.
	ModeFull Mode = "full"
)

// Strategy selects how multiple DoH upstreams are used.
type Strategy string

const (
	// StrategyRace queries all upstreams concurrently and keeps the first
	// valid answer.
	StrategyRace Strategy = "race"
	// StrategyFailover queries upstreams one by one in rotating order.
	StrategyFailover Strategy = "failover"
)

// DefaultPollutedDomains is the built-in list of domains whose DNS answers are
// known to be poisoned in mainland China (GitHub ecosystem, aligned with the
// GitHub520 domain set). Every entry also covers all of its subdomains.
// dns.google / cloudflare-dns.com are included so the foreign DoH endpoints
// themselves get clean answers: the engine's DoH client resolves their
// hostnames through the system resolver, which after a takeover is this proxy
// itself.
var DefaultPollutedDomains = []string{
	"github.com",
	"github.io",
	"githubusercontent.com",
	"githubassets.com",
	"githubcopilot.com",
	"githubstatus.com",
	"github.blog",
	"github.dev",
	"githubapp.com",
	"github.net",
	"ghcr.io",
	"github.community",
	// Fastly CDN subdomains serving GitHub pages/assets (specific subdomains
	// only — the fastly.net parent is shared with non-GitHub customers).
	"github.map.fastly.net",
	"github.global.ssl.fastly.net",
	// S3 buckets used by GitHub (specific buckets only — amazonaws.com is
	// far too broad).
	"github-cloud.s3.amazonaws.com",
	"github-com.s3.amazonaws.com",
	"github-production-release-asset-2e65be.s3.amazonaws.com",
	"github-production-repository-file-5c1aeb.s3.amazonaws.com",
	"github-production-user-asset-6210df.s3.amazonaws.com",
	"dns.google",
	"cloudflare-dns.com",
}

// Config is the root configuration structure, decoded from TOML.
type Config struct {
	// SchemaVersion tracks the config file schema; managed automatically by
	// config.EnsureSchema, do not edit by hand.
	SchemaVersion int            `toml:"schema_version"`
	Listen        []string       `toml:"listen"`
	Mode          Mode           `toml:"mode"`
	Domains       DomainsConfig  `toml:"domains"`
	Upstreams     UpstreamConfig `toml:"upstreams"`
	Cache         CacheConfig    `toml:"cache"`
	API           APIConfig      `toml:"api"`
	ECS           ECSConfig      `toml:"ecs"`
	Override      OverrideConfig `toml:"override"`
	Probe         ProbeConfig    `toml:"probe"`
	Log           LogConfig      `toml:"log"`
}

// DomainsConfig holds the polluted-domain list.
type DomainsConfig struct {
	Polluted []string `toml:"polluted"`
}

// UpstreamConfig configures the forwarding targets.
type UpstreamConfig struct {
	// DoH are the encrypted upstream endpoints used for polluted domains
	// (or everything in full mode).
	DoH []string `toml:"doh"`
	// System overrides the discovered system DNS servers for non-polluted
	// domains. Empty means "discover from the OS".
	System []string `toml:"system"`
	// Fallback are public plaintext resolvers tried when every system
	// upstream fails (dead VM gateways etc.). Empty disables the chain.
	Fallback []string `toml:"fallback"`
	// Strategy picks between race and failover for multiple DoH upstreams.
	Strategy Strategy `toml:"strategy"`
	// Timeout is the per-attempt upstream query timeout.
	Timeout time.Duration `toml:"timeout"`
	// ProxyURL is an optional HTTP(S) proxy used for DoH requests.
	ProxyURL string `toml:"proxy_url"`
	// FallbackToDoH retries via DoH when the system upstream fails.
	FallbackToDoH bool `toml:"fallback_to_doh"`
}

// CacheConfig tunes the in-memory response cache.
type CacheConfig struct {
	MaxTTL     time.Duration `toml:"max_ttl"`
	MaxEntries int           `toml:"max_entries"`
}

// OverrideConfig configures the hosts-format IP override table (GitHub520
// subscriptions etc.): listed hostnames are answered directly from the table
// instead of going through the upstream routing.
type OverrideConfig struct {
	// URLs of hosts-format lists, fetched at startup (and periodically when
	// refresh_interval > 0).
	URLs []string `toml:"urls"`
	// Files parsed at startup (local hosts files).
	Files []string `toml:"files"`
	// TTL for answers synthesized from the override table.
	TTL time.Duration `toml:"ttl"`
	// RefreshInterval re-fetches the URLs; 0 = fetch once at startup.
	RefreshInterval time.Duration `toml:"refresh_interval"`
}

// APIConfig configures the local control API used by the tray GUI.
type APIConfig struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
	Token   string `toml:"token"`
}

// ECSConfig controls EDNS Client Subnet handling.
type ECSConfig struct {
	// Strip removes the client's ECS option from queries forwarded to DoH.
	Strip bool `toml:"strip"`
	// Spoof, when set to a CIDR, injects that subnet as ECS on DoH queries
	// (useful for CDN geo tuning).
	Spoof string `toml:"spoof"`
}

// ProbeConfig tunes the optional reachability probing of polluted-domain
// answers (GitHub520-style TCP probing). Disabled by default.
type ProbeConfig struct {
	// Enabled turns probing on for DoH-sourced polluted-domain answers.
	Enabled bool `toml:"enabled"`
	// Port is the TCP port dialed to test reachability.
	Port int `toml:"port"`
	// Timeout is the per-IP dial timeout.
	Timeout time.Duration `toml:"timeout"`
	// Mode is "drop" (remove unreachable IPs, keep originals when none
	// reachable) or "prefer" (reachable first by latency, nothing dropped).
	Mode string `toml:"mode"`
	// MaxIPs caps how many IPs per family are probed per answer.
	MaxIPs int `toml:"max_ips"`
	// CacheTTL caches probe results per IP (0 disables caching).
	CacheTTL time.Duration `toml:"cache_ttl"`
}

// LogConfig tunes logging.
type LogConfig struct {
	Level          string `toml:"level"`
	VerboseQueries bool   `toml:"verbose_queries"`
	// StatsInterval is the period of the periodic stats heartbeat line;
	// 0 disables it.
	StatsInterval time.Duration `toml:"stats_interval"`
}

// Default returns a Config populated with safe defaults.
func Default() *Config {
	return &Config{
		SchemaVersion: CurrentSchemaVersion,
		Listen:        []string{"127.0.0.1:53", "[::1]:53"},
		Mode:          ModeSplit,
		Domains: DomainsConfig{
			Polluted: append([]string(nil), DefaultPollutedDomains...),
		},
		Upstreams: UpstreamConfig{
			// Order matters with strategy=failover; with the default race
			// strategy the fastest reachable endpoint wins. China-reachable
			// endpoints come first (AliDNS/DNSPod answer GitHub correctly),
			// foreign ones follow for users with proxy_url or unfiltered
			// networks.
			DoH: []string{
				"https://dns.alidns.com/dns-query",
				"https://doh.pub/dns-query",
				"https://dns.google/dns-query",
				"https://cloudflare-dns.com/dns-query",
			},
			Strategy:      StrategyRace,
			Timeout:       3 * time.Second,
			Fallback:      []string{"223.5.5.5", "119.29.29.29", "1.1.1.1"},
			FallbackToDoH: false,
		},
		Cache: CacheConfig{
			MaxTTL:     time.Hour,
			MaxEntries: 4096,
		},
		API: APIConfig{
			Enabled: true,
			Listen:  "127.0.0.1:5378",
		},
		ECS: ECSConfig{Strip: true},
		Override: OverrideConfig{
			TTL: 5 * time.Minute,
		},
		Probe: ProbeConfig{
			Port:     443,
			Timeout:  700 * time.Millisecond,
			Mode:     "drop",
			MaxIPs:   8,
			CacheTTL: time.Minute,
		},
		Log: LogConfig{Level: "info", StatsInterval: time.Minute},
	}
}

// DefaultPath returns the platform-specific default config file path.
func DefaultPath() string {
	return filepath.Join(paths.StateDir(), "config.toml")
}

// Write persists a configuration to path (creating parent directories).
func Write(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return err
	}
	// Windows: make the fresh config writable by the built-in Users group so
	// non-elevated processes can migrate it later (best effort).
	_ = ensureConfigACL(path)
	return nil
}

// Load reads and validates the configuration file at path (default location
// when empty). A missing file is an error — callers that want first-run
// bootstrapping should use LoadOrBootstrap.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found: %s (generate one with \"truedns config init\")", path)
		}
		return nil, err
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks invariants after decode/merge.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeSplit, ModeFull:
	default:
		return fmt.Errorf("invalid mode %q (want %q or %q)", c.Mode, ModeSplit, ModeFull)
	}
	switch c.Upstreams.Strategy {
	case StrategyRace, StrategyFailover:
	default:
		return fmt.Errorf("invalid upstreams.strategy %q (want %q or %q)", c.Upstreams.Strategy, StrategyRace, StrategyFailover)
	}
	if len(c.Upstreams.DoH) == 0 {
		return fmt.Errorf("at least one upstreams.doh entry is required")
	}
	for _, u := range c.Upstreams.DoH {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return fmt.Errorf("upstreams.doh entry %q must be an http(s) URL", u)
		}
	}
	if c.Upstreams.Timeout <= 0 || c.Upstreams.Timeout > time.Minute {
		return fmt.Errorf("upstreams.timeout must be within (0s, 1m]")
	}
	if c.Cache.MaxEntries <= 0 {
		return fmt.Errorf("cache.max_entries must be > 0")
	}
	if len(c.Listen) == 0 {
		return fmt.Errorf("at least one listen address is required")
	}
	if c.ECS.Spoof != "" {
		if _, _, err := net.ParseCIDR(c.ECS.Spoof); err != nil {
			return fmt.Errorf("ecs.spoof %q is not a valid CIDR: %w", c.ECS.Spoof, err)
		}
	}
	if c.Override.TTL <= 0 || c.Override.TTL > 24*time.Hour {
		return fmt.Errorf("override.ttl must be within (0s, 24h]")
	}
	if c.Override.RefreshInterval < 0 {
		return fmt.Errorf("override.refresh_interval must be >= 0 (0 = startup only)")
	}
	switch c.Probe.Mode {
	case "drop", "prefer":
	default:
		return fmt.Errorf("invalid probe.mode %q (want \"drop\" or \"prefer\")", c.Probe.Mode)
	}
	if c.Probe.Port < 1 || c.Probe.Port > 65535 {
		return fmt.Errorf("probe.port must be within [1, 65535]")
	}
	if c.Probe.Timeout <= 0 || c.Probe.Timeout > 10*time.Second {
		return fmt.Errorf("probe.timeout must be within (0s, 10s]")
	}
	if c.Probe.MaxIPs <= 0 || c.Probe.MaxIPs > 32 {
		return fmt.Errorf("probe.max_ips must be within [1, 32]")
	}
	if c.Probe.CacheTTL < 0 {
		return fmt.Errorf("probe.cache_ttl must be >= 0 (0 disables caching)")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log.level %q (want debug|info|warn|error)", c.Log.Level)
	}
	if c.Log.StatsInterval < 0 {
		return fmt.Errorf("log.stats_interval must be >= 0 (0 disables the stats heartbeat)")
	}
	return nil
}
