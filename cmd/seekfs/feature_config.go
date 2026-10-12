package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type featureConfig struct {
	Enabled bool
	Command string
	Args    []string
	Timeout time.Duration
}

func validFeatureName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}

func featureContentEnabled(cfg appConfig) bool {
	if d,ok:=cfg.Plugins["content"]; ok { return d.Installed && d.Feature.Enabled }
	if value, ok := os.LookupEnv("SEEKFS_CONTENT_SEARCH"); ok {
		return value == "1"
	}
	return cfg.Features["content"].Enabled
}

func applyFeatureConfig(cfg *appConfig, name, key, value string) error {
	value = stripFeatureConfigComment(value)
	if !validFeatureName(name) {
		return fmt.Errorf("invalid feature name %q", name)
	}
	if cfg.Features == nil {
		cfg.Features = make(map[string]featureConfig)
	}
	f := cfg.Features[name]
	var err error
	switch key {
	case "enabled":
		f.Enabled, err = strconv.ParseBool(value)
	case "command":
		f.Command, err = parseFeatureString(value)
	case "args":
		f.Args, err = parseFeatureArgs(value)
	case "timeout_seconds":
		var n int
		n, err = strconv.Atoi(value)
		if err == nil && (n < 1 || n > 60) {
			err = fmt.Errorf("must be between 1 and 60")
		}
		f.Timeout = time.Duration(n) * time.Second
	default:
		err = fmt.Errorf("unknown key")
	}
	if name == "content" && key != "enabled" {
		err = fmt.Errorf("built-in content accepts only enabled; use [content] for its scope")
	}
	if err != nil {
		return fmt.Errorf("features.%s.%s: %w", name, key, err)
	}
	cfg.Features[name] = f
	return nil
}

func validateFeatureConfig(features map[string]featureConfig) error {
	for name, f := range features {
		if !validFeatureName(name) {
			return fmt.Errorf("invalid feature name %q", name)
		}
		if name != "content" && f.Enabled && !filepath.IsAbs(f.Command) {
			return fmt.Errorf("features.%s.command must be an absolute executable path", name)
		}
	}
	return nil
}

func parseFeatureString(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		if strings.ContainsRune(s[1:len(s)-1], '\'') {
			return "", fmt.Errorf("invalid literal string")
		}
		return s[1 : len(s)-1], nil
	}
	if len(s) < 2 || s[0] != '"' {
		return "", fmt.Errorf("expected a quoted string")
	}
	return strconv.Unquote(s)
}

func stripFeatureConfigComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\'' {
			quote := s[i]
			i++
			for i < len(s) {
				if quote == '"' && s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == quote {
					break
				}
				i++
			}
			continue
		}
		if s[i] == '#' {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// The feature config supports single-line TOML string arrays, including commas
// inside quoted arguments. Literal single quotes preserve Windows paths.
func parseFeatureArgs(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("expected a string array")
	}
	s = strings.TrimSpace(s[1 : len(s)-1])
	var args []string
	for s != "" {
		if s[0] != '\'' && s[0] != '"' {
			return nil, fmt.Errorf("expected a quoted argument")
		}
		quote := s[0]
		end := 1
		for end < len(s) {
			if quote == '"' && s[end] == '\\' {
				end += 2
				continue
			}
			if s[end] == quote {
				break
			}
			end++
		}
		if end >= len(s) {
			return nil, fmt.Errorf("unterminated argument")
		}
		arg, err := parseFeatureString(s[:end+1])
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		s = strings.TrimSpace(s[end+1:])
		if s == "" {
			break
		}
		if s[0] != ',' {
			return nil, fmt.Errorf("expected comma between arguments")
		}
		s = strings.TrimSpace(s[1:])
	}
	return args, nil
}
