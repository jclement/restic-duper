// Package config loads and validates the restic-duper configuration file.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be written as "30s" / "2h" in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the top-level configuration.
type Config struct {
	// ResticBinary overrides the restic executable path. Defaults to "restic"
	// resolved via PATH.
	ResticBinary  string        `yaml:"restic_binary"`
	Notifications Notifications `yaml:"notifications"`
	Pairs         []Pair        `yaml:"pairs"`
}

type Notifications struct {
	Webhook *Webhook `yaml:"webhook"`
}

// Webhook describes a generic HTTP webhook fired when a run finishes.
type Webhook struct {
	URL       string            `yaml:"url"`
	Method    string            `yaml:"method"`     // default POST
	Headers   map[string]string `yaml:"headers"`    // extra headers
	OnSuccess bool              `yaml:"on_success"` // default false
	OnFailure *bool             `yaml:"on_failure"` // default true
	Timeout   Duration          `yaml:"timeout"`    // default 30s
}

func (w *Webhook) FireOnFailure() bool { return w.OnFailure == nil || *w.OnFailure }

// Repo describes one restic repository and how to unlock it.
// Exactly one of Password, PasswordFile, or PasswordCommand must be set.
type Repo struct {
	Repo            string            `yaml:"repo"`
	Password        string            `yaml:"password"`
	PasswordFile    string            `yaml:"password_file"`
	PasswordCommand string            `yaml:"password_command"`
	Env             map[string]string `yaml:"env"` // backend credentials, e.g. AWS_ACCESS_KEY_ID
}

// Pair is one copy job: replicate snapshots From -> To.
type Pair struct {
	Name string `yaml:"name"`
	From Repo   `yaml:"from"`
	To   Repo   `yaml:"to"`
	// Snapshots selects what to copy: "latest" (default) or "all".
	// Use copy_args (e.g. --host, --tag, --path) to narrow further.
	Snapshots string   `yaml:"snapshots"`
	CopyArgs  []string `yaml:"copy_args"`
	Timeout   Duration `yaml:"timeout"` // 0 = no timeout
	// AllowEmpty permits a run in which restic copy matched zero snapshots.
	// By default that is treated as a failure, because a healthy "latest"
	// copy always saves or skips at least one snapshot — zero matches means
	// the source is empty or the filters match nothing.
	AllowEmpty bool `yaml:"allow_empty"`
}

// Load reads, expands, and validates a config file.
//
// ${VAR} expansion happens on the parsed YAML scalar values, never on the
// raw file text: expanded values are always plain strings, so secrets
// containing '#', ':', or newlines cannot truncate a value or inject YAML
// structure, and ${VAR} inside comments is ignored.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.expand(); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// expand applies ExpandEnv to every user-supplied string value in the
// already-parsed config.
func (c *Config) expand() error {
	fields := []*string{&c.ResticBinary}
	if w := c.Notifications.Webhook; w != nil {
		fields = append(fields, &w.URL)
		if err := expandMap(w.Headers); err != nil {
			return err
		}
	}
	for i := range c.Pairs {
		p := &c.Pairs[i]
		for _, r := range []*Repo{&p.From, &p.To} {
			fields = append(fields, &r.Repo, &r.Password, &r.PasswordFile, &r.PasswordCommand)
			if err := expandMap(r.Env); err != nil {
				return err
			}
		}
		for j := range p.CopyArgs {
			fields = append(fields, &p.CopyArgs[j])
		}
	}
	for _, f := range fields {
		v, err := ExpandEnv(*f)
		if err != nil {
			return err
		}
		*f = v
	}
	return nil
}

func expandMap(m map[string]string) error {
	for k, v := range m {
		nv, err := ExpandEnv(v)
		if err != nil {
			return err
		}
		m[k] = nv
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// ExpandEnv replaces ${VAR} references with values from the process
// environment. Unlike os.ExpandEnv it leaves bare $VAR untouched (passwords
// often contain '$'), and it errors on references to unset variables rather
// than silently substituting "".
func ExpandEnv(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config references unset environment variable(s): %v", missing)
	}
	return out, nil
}

// Validate checks the whole config for consistency.
func (c *Config) Validate() error {
	if len(c.Pairs) == 0 {
		return fmt.Errorf("no pairs defined")
	}
	seen := map[string]bool{}
	for i := range c.Pairs {
		p := &c.Pairs[i]
		if p.Name == "" {
			return fmt.Errorf("pairs[%d]: name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate pair name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Snapshots == "" {
			p.Snapshots = "latest"
		}
		if p.Snapshots != "latest" && p.Snapshots != "all" {
			return fmt.Errorf("pair %q: snapshots must be \"latest\" or \"all\", got %q", p.Name, p.Snapshots)
		}
		if err := p.From.validate(); err != nil {
			return fmt.Errorf("pair %q: from: %w", p.Name, err)
		}
		if err := p.To.validate(); err != nil {
			return fmt.Errorf("pair %q: to: %w", p.Name, err)
		}
		if p.From.Repo == p.To.Repo {
			return fmt.Errorf("pair %q: from and to are the same repository (%s)", p.Name, p.From.Repo)
		}
		// restic copy runs as a single process with a single environment, so
		// the same backend variable cannot hold different values for the two
		// sides. Catch that early with a useful message.
		for k, fv := range p.From.Env {
			if tv, ok := p.To.Env[k]; ok && tv != fv {
				return fmt.Errorf("pair %q: env variable %s is set to different values on from and to; "+
					"restic copy uses one environment for both repositories, so backend credentials must be shared "+
					"(consider the rclone backend or per-repo credential files)", p.Name, k)
			}
		}
	}
	if w := c.Notifications.Webhook; w != nil {
		if w.URL == "" {
			return fmt.Errorf("notifications.webhook: url is required")
		}
		if w.Method == "" {
			w.Method = "POST"
		}
		if w.Timeout == 0 {
			w.Timeout = Duration(30 * time.Second)
		}
	}
	return nil
}

func (r *Repo) validate() error {
	if r.Repo == "" {
		return fmt.Errorf("repo is required")
	}
	n := 0
	for _, s := range []string{r.Password, r.PasswordFile, r.PasswordCommand} {
		if s != "" {
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("one of password, password_file, or password_command is required")
	}
	if n > 1 {
		return fmt.Errorf("password, password_file, and password_command are mutually exclusive")
	}
	return nil
}

// MergedEnv combines from/to backend env for a pair. Validate has already
// guaranteed there are no conflicting values.
func (p *Pair) MergedEnv() map[string]string {
	out := map[string]string{}
	for k, v := range p.From.Env {
		out[k] = v
	}
	for k, v := range p.To.Env {
		out[k] = v
	}
	return out
}
