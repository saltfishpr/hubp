package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server     ServerConfig     `json:"server"`
	Cache      CacheConfig      `json:"cache"`
	Registries []RegistryConfig `json:"registries"`
}

type ServerConfig struct {
	Listen            string   `json:"listen"`
	PublicURL         string   `json:"public_url"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}

type CacheConfig struct {
	Enabled     bool     `json:"enabled"`
	Dir         string   `json:"dir"`
	ManifestTTL Duration `json:"manifest_ttl"`
}

type RegistryConfig struct {
	Name                       string   `json:"name"`
	PathPrefix                 string   `json:"path_prefix"`
	Upstream                   string   `json:"upstream"`
	HTTPSProxy                 string   `json:"https_proxy"`
	AllowedAuthHosts           []string `json:"allowed_auth_hosts"`
	Username                   string   `json:"username"`
	Password                   string   `json:"password"`
	ForwardClientAuthorization bool     `json:"forward_client_authorization"`
	TLSInsecureSkipVerify      bool     `json:"tls_insecure_skip_verify"`
}

var (
	envPattern          = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
	registryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// Load reads a JSON configuration file, expands ${ENV_NAME} placeholders in
// JSON strings, rejects unknown fields, applies defaults, and validates the
// resulting configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data = expandEnvironment(data)

	cfg := defaults()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func expandEnvironment(data []byte) []byte {
	return envPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		quoted := strconv.Quote(os.Getenv(name))
		return []byte(quoted[1 : len(quoted)-1])
	})
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:            ":8080",
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			ShutdownTimeout:   Duration(15 * time.Second),
		},
		Cache: CacheConfig{
			Enabled: false,
		},
	}
}

func (c *Config) Validate() error {
	var problems []error
	if strings.TrimSpace(c.Server.Listen) == "" {
		problems = append(problems, errors.New("server.listen must not be empty"))
	}
	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			problems = append(problems, fmt.Errorf("server.public_url must be an absolute URL: %q", c.Server.PublicURL))
		} else {
			c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")
		}
	}
	if c.Cache.Enabled && strings.TrimSpace(c.Cache.Dir) == "" {
		problems = append(problems, errors.New("cache.dir must not be empty when cache is enabled"))
	}
	if c.Cache.ManifestTTL.Value() < 0 {
		problems = append(problems, errors.New("cache.manifest_ttl must be >= 0"))
	}
	if len(c.Registries) == 0 {
		problems = append(problems, errors.New("at least one registry must be configured"))
	}

	seenNames := map[string]struct{}{}
	seenPrefixes := map[string]struct{}{}
	defaultCount := 0
	for i := range c.Registries {
		r := &c.Registries[i]
		r.Name = strings.TrimSpace(r.Name)
		r.PathPrefix = strings.Trim(strings.TrimSpace(r.PathPrefix), "/")
		r.Upstream = strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
		r.HTTPSProxy = strings.TrimSpace(r.HTTPSProxy)

		if !registryNamePattern.MatchString(r.Name) {
			problems = append(problems, fmt.Errorf("registries[%d].name is invalid: %q", i, r.Name))
		}
		if _, ok := seenNames[r.Name]; ok {
			problems = append(problems, fmt.Errorf("duplicate registry name %q", r.Name))
		}
		seenNames[r.Name] = struct{}{}
		if _, ok := seenPrefixes[r.PathPrefix]; ok {
			problems = append(problems, fmt.Errorf("duplicate registry path_prefix %q", r.PathPrefix))
		}
		seenPrefixes[r.PathPrefix] = struct{}{}
		if r.PathPrefix == "" {
			defaultCount++
		}

		u, err := url.Parse(r.Upstream)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			problems = append(problems, fmt.Errorf("registries[%d].upstream must be an HTTPS origin without a path: %q", i, r.Upstream))
		}
		if r.HTTPSProxy != "" {
			p, err := url.Parse(r.HTTPSProxy)
			if err != nil || p.Scheme == "" || p.Host == "" {
				problems = append(problems, fmt.Errorf("registries[%d].https_proxy is invalid: %q", i, r.HTTPSProxy))
			}
		}
		if (r.Username == "") != (r.Password == "") {
			problems = append(problems, fmt.Errorf("registries[%d] must set both username and password, or neither", i))
		}
		if len(r.AllowedAuthHosts) == 0 && u != nil && u.Hostname() != "" {
			r.AllowedAuthHosts = []string{strings.ToLower(u.Hostname())}
		}
		for j := range r.AllowedAuthHosts {
			r.AllowedAuthHosts[j] = strings.ToLower(strings.TrimSpace(r.AllowedAuthHosts[j]))
			if r.AllowedAuthHosts[j] == "" {
				problems = append(problems, fmt.Errorf("registries[%d].allowed_auth_hosts contains an empty value", i))
			}
		}
	}
	if defaultCount != 1 {
		problems = append(problems, fmt.Errorf("exactly one registry must have an empty path_prefix; got %d", defaultCount))
	}
	return errors.Join(problems...)
}

func (c *Config) SortedRegistries() []RegistryConfig {
	out := append([]RegistryConfig(nil), c.Registries...)
	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i].PathPrefix) > len(out[j].PathPrefix)
	})
	return out
}
