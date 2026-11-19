package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Source is one directory of transcripts. Two sources may share a kind but
// not a base name, which starts each of its files' names in the archive.
type Source struct {
	Kind string `toml:"kind" json:"kind"`
	Dir  string `toml:"dir" json:"dir"`
}

// Name is the source's prefix in the archive: claude/projects, codex/sessions.
func (s Source) Name() string { return s.Kind + "/" + filepath.Base(s.Dir) }

// Config is roost's whole configuration. Only ArchiveDir has no default.
type Config struct {
	ArchiveDir       string   `toml:"archive_dir" json:"archive_dir"`
	DataDir          string   `toml:"data_dir" json:"data_dir"`
	IdleHours        int      `toml:"idle_hours" json:"idle_hours"`
	BundleMaxMB      int      `toml:"bundle_max_mb" json:"bundle_max_mb"`
	MaxBundlesPerRun int      `toml:"max_bundles_per_run" json:"max_bundles_per_run"`
	ExcludeProjects  []string `toml:"exclude_projects" json:"exclude_projects"`
	Release          bool     `toml:"release" json:"release"`
	Notify           bool     `toml:"notify" json:"notify"`
	Sources          []Source `toml:"sources" json:"sources"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// DefaultPath is where the config lives unless ROOST_CONFIG or --config says.
func DefaultPath() string {
	return envOr("ROOST_CONFIG", filepath.Join(envOr("XDG_CONFIG_HOME", filepath.Join(home(), ".config")), "roost", "config.toml"))
}

// defaultSources are the folders Claude Code and Codex write transcripts to.
func defaultSources() []Source {
	claude := envOr("CLAUDE_CONFIG_DIR", filepath.Join(home(), ".claude"))
	codex := envOr("CODEX_HOME", filepath.Join(home(), ".codex"))
	return []Source{
		{Kind: "claude", Dir: filepath.Join(claude, "projects")},
		{Kind: "codex", Dir: filepath.Join(codex, "sessions")},
		{Kind: "codex", Dir: filepath.Join(codex, "archived_sessions")},
	}
}

// Defaults is the configuration before the file is read.
func Defaults() Config {
	return Config{
		DataDir:          filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(home(), ".local", "share")), "roost"),
		IdleHours:        24,
		BundleMaxMB:      256,
		MaxBundlesPerRun: 2,
		Release:          true,
		Notify:           runtime.GOOS == "darwin",
	}
}

// Load reads the config file at path. Unknown keys are refused, so a typo
// never silently turns a setting off. Without configured sources, the default
// folders that exist are used, so a machine with only one agent still
// completes its passes.
func Load(path string) (Config, error) {
	cfg := Defaults()
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("no config at %s; create it with at least: archive_dir = \"/path/to/a/synced/folder\"", path)
	}
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return cfg, fmt.Errorf("config: unknown key %s", undecoded[0])
	}
	if !md.IsDefined("sources") {
		for _, src := range defaultSources() {
			if _, err := os.Stat(src.Dir); err == nil {
				cfg.Sources = append(cfg.Sources, src)
			}
		}
	}
	cfg.ArchiveDir = expand(cfg.ArchiveDir)
	cfg.DataDir = expand(cfg.DataDir)
	for i := range cfg.Sources {
		cfg.Sources[i].Dir = expand(cfg.Sources[i].Dir)
	}
	return cfg, cfg.validate()
}

func expand(p string) string {
	if p == "~" {
		return home()
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home(), rest)
	}
	return p
}

func inside(child, parent string) bool {
	return strings.HasPrefix(filepath.Clean(child)+"/", filepath.Clean(parent)+"/")
}

func (c Config) validate() error {
	var errs []error
	switch {
	case c.ArchiveDir == "":
		errs = append(errs, errors.New("archive_dir is required"))
	case !filepath.IsAbs(c.ArchiveDir):
		errs = append(errs, errors.New("archive_dir must be an absolute path or start with ~/"))
	case inside(c.ArchiveDir, c.DataDir) || inside(c.DataDir, c.ArchiveDir):
		errs = append(errs, errors.New("archive_dir and data_dir must not contain each other"))
	}
	if !filepath.IsAbs(c.DataDir) {
		errs = append(errs, errors.New("data_dir must be an absolute path or start with ~/"))
	}
	if c.IdleHours < 1 {
		errs = append(errs, errors.New("idle_hours must be at least 1"))
	}
	if c.BundleMaxMB < 1 {
		errs = append(errs, errors.New("bundle_max_mb must be at least 1"))
	}
	if c.MaxBundlesPerRun < 1 {
		errs = append(errs, errors.New("max_bundles_per_run must be at least 1"))
	}
	if len(c.Sources) == 0 {
		errs = append(errs, errors.New("no transcript folders found; set [[sources]]"))
	}
	var names []string
	for i, src := range c.Sources {
		if src.Kind != "claude" && src.Kind != "codex" {
			errs = append(errs, fmt.Errorf("sources[%d].kind must be claude or codex", i))
		}
		if !filepath.IsAbs(src.Dir) {
			errs = append(errs, fmt.Errorf("sources[%d].dir must be an absolute path or start with ~/", i))
		}
		if slices.Contains(names, src.Name()) {
			errs = append(errs, fmt.Errorf("sources[%d] repeats %s", i, src.Name()))
		}
		names = append(names, src.Name())
	}
	return errors.Join(errs...)
}
