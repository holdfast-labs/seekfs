package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

type pluginDefinition struct {
	Installed bool
	Feature   featureConfig
	Content   contentScopeConfig
}

type pluginSettings struct{ Definitions map[string]pluginDefinition }

func loadPluginDefinitions(path string) (map[string]pluginDefinition, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]pluginDefinition{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parsePluginDefinitions(string(data))
}

func parsePluginDefinitions(text string) (map[string]pluginDefinition, error) {
	defs := make(map[string]pluginDefinition)
	name := ""
	seen := make(map[string]bool)
	for number, line := range strings.Split(text, "\n") {
		line = stripFeatureConfigComment(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		fail := func(err error) (map[string]pluginDefinition, error) {
			return nil, fmt.Errorf("plugin.toml line %d: %w", number+1, err)
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			var ok bool
			name, ok = strings.CutPrefix(section, "plugins.")
			if !ok || !validFeatureName(name) {
				return fail(fmt.Errorf("expected [plugins.<name>]"))
			}
			if _, exists := defs[name]; exists {
				return fail(fmt.Errorf("duplicate plugin section %s", name))
			}
			defs[name] = pluginDefinition{Installed: true, Feature: featureConfig{Enabled: true}}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || name == "" {
			return fail(fmt.Errorf("expected a plugin key=value"))
		}
		if seen[name+"."+key] {
			return fail(fmt.Errorf("duplicate key %s.%s", name, key))
		}
		seen[name+"."+key] = true
		d := defs[name]
		var err error
		switch key {
		case "installed":
			d.Installed, err = strconv.ParseBool(value)
		case "scope", "roots", "exclude", "exts", "git", "system_excludes", "budget_bytes":
			if name != "content" {
				err = fmt.Errorf("content-only setting %s", key)
				break
			}
			switch key {
			case "scope":
				d.Content.Mode, err = parseFeatureString(value)
				if err == nil {
					if _, ok := parseContentScopeMode(d.Content.Mode); !ok {
						err = fmt.Errorf("invalid scope")
					}
				}
			case "roots", "exclude", "exts":
				var values []string
				values, err = parseFeatureArgs(value)
				if err == nil && values == nil {
					values = []string{}
				}
				if key == "roots" {
					d.Content.Roots = values
				}
				if key == "exclude" {
					d.Content.Exclude = values
				}
				if key == "exts" {
					d.Content.Exts = values
				}
			case "git", "system_excludes":
				var b bool
				b, err = strconv.ParseBool(value)
				if key == "git" {
					d.Content.Git = &b
				} else {
					d.Content.SystemExcludes = &b
				}
			case "budget_bytes":
				d.Content.BudgetBytes, err = strconv.ParseInt(value, 10, 64)
				if err == nil && d.Content.BudgetBytes <= 0 {
					err = fmt.Errorf("budget must be positive")
				}
			}
		default:
			cfg := appConfig{Features: map[string]featureConfig{name: d.Feature}}
			err = applyFeatureConfig(&cfg, name, key, value)
			d.Feature = cfg.Features[name]
		}
		if err != nil {
			return fail(err)
		}
		defs[name] = d
	}
	for name, d := range defs {
		if !d.Installed {
			d.Feature.Enabled = false
			defs[name] = d
		}
		if err := validateFeatureConfig(map[string]featureConfig{name: d.Feature}); err != nil {
			return nil, err
		}
		for _, root := range d.Content.Roots {
			if !filepath.IsAbs(root) {
				return nil, fmt.Errorf("plugin %s root must be absolute: %s", name, root)
			}
		}
	}
	return defs, nil
}

func configuredFeatureMap(cfg appConfig, plugins map[string]pluginDefinition) map[string]featureConfig {
	out := make(map[string]featureConfig, len(cfg.Features)+len(plugins))
	for name, f := range cfg.Features {
		out[name] = f
	}
	for name, d := range plugins {
		f := d.Feature
		if !d.Installed {
			f.Enabled = false
		}
		out[name] = f
	}
	return out
}

func formatPluginDefinition(name string, d pluginDefinition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[plugins.%s]\ninstalled = %t\nenabled = %t\n", name, d.Installed, d.Installed && d.Feature.Enabled)
	if d.Feature.Command != "" {
		fmt.Fprintf(&b, "command = %s\n", strconvQuote(d.Feature.Command))
	}
	if d.Feature.Args != nil {
		fmt.Fprintf(&b, "args = %s\n", formatStringArray(d.Feature.Args))
	}
	if d.Feature.Timeout > 0 {
		fmt.Fprintf(&b, "timeout_seconds = %d\n", int(d.Feature.Timeout.Seconds()))
	}
	sc := d.Content
	if sc.Mode != "" {
		fmt.Fprintf(&b, "scope = %s\n", strconvQuote(sc.Mode))
	}
	for _, item := range []struct {
		key    string
		values []string
	}{{"roots", sc.Roots}, {"exclude", sc.Exclude}, {"exts", sc.Exts}} {
		if item.values != nil {
			fmt.Fprintf(&b, "%s = %s\n", item.key, formatStringArray(item.values))
		}
	}
	if sc.Git != nil {
		fmt.Fprintf(&b, "git = %t\n", *sc.Git)
	}
	if sc.SystemExcludes != nil {
		fmt.Fprintf(&b, "system_excludes = %t\n", *sc.SystemExcludes)
	}
	if sc.BudgetBytes > 0 {
		fmt.Fprintf(&b, "budget_bytes = %d\n", sc.BudgetBytes)
	}
	return b.String()
}

// Change only the selected section; retain other plugins and user comments.
func replacePluginSection(text, name, replacement string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		line = stripFeatureConfigComment(strings.TrimSpace(line))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if start >= 0 {
				end = i
				break
			}
			if line == "[plugins."+name+"]" {
				start = i
			}
		}
	}
	if start < 0 {
		return strings.TrimRight(text, "\r\n") + "\n\n" + replacement
	}
	// Keep section comments even when canonicalizing its managed keys.
	var comments []string
	for _, line := range lines[start+1 : end] {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			comments = append(comments, line)
		}
	}
	middle := strings.TrimRight(replacement, "\n")
	if len(comments) > 0 {
		middle += "\n" + strings.Join(comments, "\n")
	}
	return strings.TrimRight(strings.Join(append(append(lines[:start], middle), lines[end:]...), "\n"), "\n") + "\n"
}

func updatePluginFile(path, name string, edit func(pluginDefinition, bool) (pluginDefinition, error)) error {
	if !validFeatureName(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	lock, err := acquireContentVolumeLock(path)
	if err != nil {
		return fmt.Errorf("plugin configuration is being edited: %w", err)
	}
	defer lock.release()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defs, err := parsePluginDefinitions(string(data))
	if err != nil {
		return err
	}
	d, exists := defs[name]
	d, err = edit(d, exists)
	if err != nil {
		return err
	}
	text := replacePluginSection(string(data), name, formatPluginDefinition(name, d))
	if _, err := parsePluginDefinitions(text); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plugin-*.tmp")
	if err != nil {
		return err
	}
	tempPath := tmp.Name()
	defer os.Remove(tempPath)
	if _, err = tmp.WriteString(text); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func pluginNames(defs map[string]pluginDefinition) []string {
	names := make([]string, 0, len(defs)+1)
	names = append(names, "content")
	for name := range defs {
		if name != "content" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
