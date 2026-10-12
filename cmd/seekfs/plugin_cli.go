package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var pluginServiceCall = callService

type pluginCommandResponse struct {
	OK      bool           `json:"ok"`
	Config  string         `json:"config"`
	Service bool           `json:"service"`
	Message string         `json:"message,omitempty"`
	Plugins []pluginStatus `json:"plugins"`
}

func cmdPlugin(args []string) error { return runPluginCommand(args, os.Stdout) }

func runPluginCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	op := args[0]
	args = args[1:]
	if op == "help" || op == "-h" || op == "--help" {
		fmt.Fprintln(out, `seekfs plugin list [--json]
seekfs plugin add content [--scope auto|explicit] [--root PATH] [--wait]
seekfs plugin add NAME --command C:\Tools\companion.exe [--arg VALUE]
seekfs plugin remove|enable|disable NAME
seekfs plugin config NAME [--scope MODE] [--root PATH] [--budget-bytes N]
seekfs plugin doctor [NAME] [--wait] [--timeout 10m] [--json]
seekfs plugin reload
seekfs plugin path
Common flags: --config SEEKFS_TOML, --plugin-config PLUGIN_TOML, --pipe PIPE.
Plugin settings are stored separately in plugin.toml. Adding starts background
activation in a running service; --wait waits for readiness.`)
		return nil
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("plugin "+op, flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "seekfs.toml path")
	pluginPath := fs.String("plugin-config", "", "plugin.toml path (defaults beside service config)")
	pipe := fs.String("pipe", defaultServicePipe, "resident service pipe")
	jsonOut := fs.Bool("json", false, "JSON output")
	wait := fs.Bool("wait", false, "wait for plugin readiness")
	timeout := fs.Duration("timeout", 10*time.Minute, "readiness wait timeout")
	command := fs.String("command", "", "companion executable")
	requestTimeout := fs.Int("timeout-seconds", 5, "companion request timeout")
	scope := fs.String("scope", "", "content scope: auto, explicit or off")
	budget := fs.Int64("budget-bytes", 0, "content text budget")
	git := fs.Bool("git", true, "include detected git worktrees")
	system := fs.Bool("system-excludes", true, "exclude generated/system directories")
	var roots, excludes, exts, commandArgs stringList
	fs.Var(&roots, "root", "content root; repeatable")
	fs.Var(&excludes, "exclude", "excluded content path; repeatable")
	fs.Var(&exts, "ext", "content extension; repeatable")
	fs.Var(&commandArgs, "arg", "companion argument; repeatable (use --arg=--flag)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		if name != "" || fs.NArg() != 1 {
			return fmt.Errorf("unexpected plugin arguments")
		}
		name = fs.Arg(0)
	}
	if name == "content-search" {
		name = "content"
	}
	if name != "" && !validFeatureName(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	changed := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { changed[f.Name] = true })
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if !changed["pipe"] && cfg.ServicePipe != "" {
		*pipe = cfg.ServicePipe
	}
	info, serviceErr := pluginServiceCall(*pipe, serviceRequest{Command: "info"})
	if *pluginPath == "" {
		if !changed["config"] && serviceErr == nil && info.PluginConfig != "" {
			*pluginPath = info.PluginConfig
		} else {
			*pluginPath = cfg.PluginPath
		}
	}
	if *pluginPath == "" {
		*pluginPath = filepath.Join(filepath.Dir(defaultConfigPath()), "plugin.toml")
	}
	*pluginPath, err = filepath.Abs(*pluginPath)
	if err != nil {
		return err
	}
	if op == "path" {
		if *jsonOut {
			return json.NewEncoder(out).Encode(map[string]string{"path": *pluginPath})
		}
		fmt.Fprintln(out, *pluginPath)
		return nil
	}
	defs, err := loadPluginDefinitions(*pluginPath)
	if err != nil {
		return err
	}
	settingsChanged := false
	for _, key := range []string{"command", "arg", "timeout-seconds", "scope", "root", "exclude", "ext", "git", "system-excludes", "budget-bytes"} {
		settingsChanged = settingsChanged || changed[key]
	}
	mutating := op == "add" || op == "remove" || op == "enable" || op == "disable" || op == "config" && settingsChanged
	if settingsChanged && (op == "remove" || op == "enable" || op == "disable") {
		return fmt.Errorf("use plugin config to change settings")
	}
	if *wait && *timeout <= 0 {
		return fmt.Errorf("wait timeout must be positive")
	}
	if mutating {
		if name == "" {
			return fmt.Errorf("plugin %s requires a name", op)
		}
		if serviceErr == nil && info.PluginConfig != "" && !samePath(info.PluginConfig, *pluginPath) {
			return fmt.Errorf("running service uses %s; select that plugin file", info.PluginConfig)
		}
		if name != "content" && (changed["scope"] || changed["root"] || changed["exclude"] || changed["ext"] || changed["git"] || changed["system-excludes"] || changed["budget-bytes"]) {
			return fmt.Errorf("content settings require plugin content")
		}
		if name == "content" && (changed["command"] || changed["arg"] || changed["timeout-seconds"]) {
			return fmt.Errorf("content is built in; no executable is required")
		}
		for i, root := range roots {
			roots[i], err = filepath.Abs(root)
			if err != nil {
				return err
			}
		}
		for i, path := range excludes {
			excludes[i], err = filepath.Abs(path)
			if err != nil {
				return err
			}
		}
		if changed["command"] {
			*command, err = filepath.Abs(*command)
			if err != nil {
				return err
			}
			st, err := os.Stat(*command)
			if err != nil {
				return err
			}
			if st.IsDir() {
				return fmt.Errorf("plugin command must be a file")
			}
		}
		err = updatePluginFile(*pluginPath, name, func(d pluginDefinition, exists bool) (pluginDefinition, error) {
			if !exists {
				if legacy, ok := cfg.Features[name]; ok {
					d = pluginDefinition{Installed: true, Feature: legacy}
					exists = true
				}
			}
			if op == "remove" {
				return pluginDefinition{Installed: false}, nil
			}
			if (op == "enable" || op == "disable" || op == "config") && (!exists || !d.Installed) {
				return d, fmt.Errorf("plugin %s is not installed; use plugin add", name)
			}
			if op == "add" {
				d.Installed, d.Feature.Enabled = true, true
			}
			if op == "enable" {
				d.Feature.Enabled = true
			}
			if op == "disable" {
				d.Feature.Enabled = false
			}
			if changed["command"] {
				d.Feature.Command = *command
			}
			if changed["arg"] {
				d.Feature.Args = append([]string(nil), commandArgs...)
			}
			if changed["timeout-seconds"] {
				if *requestTimeout < 1 || *requestTimeout > 60 {
					return d, fmt.Errorf("timeout-seconds must be 1–60")
				}
				d.Feature.Timeout = time.Duration(*requestTimeout) * time.Second
			}
			if changed["scope"] {
				if _, ok := parseContentScopeMode(*scope); !ok {
					return d, fmt.Errorf("invalid content scope")
				}
				d.Content.Mode = *scope
			}
			if changed["root"] {
				d.Content.Roots = append([]string{}, roots...)
				if !changed["scope"] {
					d.Content.Mode = "explicit"
				}
			}
			if changed["exclude"] {
				d.Content.Exclude = append([]string{}, excludes...)
			}
			if changed["ext"] {
				d.Content.Exts = append([]string{}, exts...)
			}
			if changed["budget-bytes"] {
				if *budget <= 0 {
					return d, fmt.Errorf("budget-bytes must be positive")
				}
				d.Content.BudgetBytes = *budget
			}
			if changed["git"] {
				d.Content.Git = git
			}
			if changed["system-excludes"] {
				d.Content.SystemExcludes = system
			}
			if name != "content" && d.Feature.Command == "" {
				return d, fmt.Errorf("unknown plugin %s; supply --command for a compatible companion", name)
			}
			return d, nil
		})
		if err != nil {
			return err
		}
		defs, err = loadPluginDefinitions(*pluginPath)
		if err != nil {
			return err
		}
	}
	if op == "config" && !mutating {
		if name == "" {
			return fmt.Errorf("plugin config requires a name")
		}
		d, ok := defs[name]
		if !ok || !d.Installed {
			return fmt.Errorf("plugin %s is not installed", name)
		}
		if *jsonOut {
			return json.NewEncoder(out).Encode(map[string]string{"config": *pluginPath, "name": name, "toml": formatPluginDefinition(name, d)})
		}
		fmt.Fprint(out, formatPluginDefinition(name, d))
		return nil
	}
	if !mutating && op != "list" && op != "doctor" && op != "reload" {
		return fmt.Errorf("unknown plugin command %q", op)
	}
	response := pluginCommandResponse{OK: true, Config: *pluginPath, Service: serviceErr == nil, Plugins: localPluginStatuses(cfg, defs)}
	if serviceErr == nil && info.PluginConfig != "" && !samePath(info.PluginConfig, *pluginPath) {
		response.Service = false
		response.Message = "selected plugin file is not the running service's registry"
	}
	if mutating || op == "reload" {
		if serviceErr != nil {
			response.Service = false
			response.Message = "saved; service offline — start seekfs to activate plugins"
			if op == "reload" {
				response.OK = false
				response.Message = "service offline; start seekfs to load plugin.toml"
			}
		} else if info.PluginAPI < 1 {
			response.Message = "saved; running service needs a binary upgrade/restart for live plugins"
		} else {
			applied, err := pluginServiceCall(*pipe, serviceRequest{Command: "plugin-reload", PluginConfig: *pluginPath})
			if err != nil {
				response.OK = false
				response.Message = "saved; activation failed: " + err.Error()
			} else if !applied.OK {
				response.OK = false
				response.Message = "saved; " + applied.Message
			} else {
				response.Plugins = applied.Plugins
				response.Message = "saved; plugin activation/indexing runs in the background"
			}
		}
	} else if response.Service && info.PluginAPI >= 1 {
		live, err := pluginServiceCall(*pipe, serviceRequest{Command: "plugin-list"})
		if err == nil && live.OK {
			response.Plugins = live.Plugins
		} else {
			response.OK = false
			if err != nil {
				response.Message = err.Error()
			} else {
				response.Message = live.Message
			}
		}
	} else if op == "doctor" {
		response.OK = false
		response.Message = "service unavailable or needs a plugin-aware binary; use seekfs launch/restart"
	}
	if name != "" {
		response.Plugins = filterPluginStatuses(response.Plugins, name)
		if len(response.Plugins) == 0 {
			response.OK = false
			response.Message = "plugin not found: " + name
		}
	}
	if *wait && (op == "add" || op == "enable" || op == "config" || op == "doctor" || op == "reload") && response.OK {
		if !response.Service || info.PluginAPI < 1 {
			response.OK = false
			response.Message = "configuration saved, but no plugin-aware service is available to wait for"
		} else {
			response = waitForPlugins(*pipe, *pluginPath, name, *timeout, out, *jsonOut)
		}
	}
	if op == "doctor" && !pluginsReady(response.Plugins) && (name != "" || enabledPlugins(response.Plugins)) {
		response.OK = false
		if response.Message == "" {
			response.Message = "plugins are not ready; use plugin doctor --wait to monitor indexing"
		}
	}
	if *jsonOut {
		if err := json.NewEncoder(out).Encode(response); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "Plugin configuration:", response.Config)
		for _, p := range response.Plugins {
			fmt.Fprintf(out, "%-18s %-10s installed=%t enabled=%t %s", p.Name, p.Kind, p.Installed, p.Enabled, p.State)
			if p.Progress != nil && p.Progress.Total > 0 {
				fmt.Fprintf(out, " %d/%d %s", p.Progress.Current, p.Progress.Total, p.Progress.Unit)
			}
			if p.Error != "" {
				fmt.Fprint(out, " — ", p.Error)
			}
			fmt.Fprintln(out)
		}
		if response.Message != "" {
			fmt.Fprintln(out, response.Message)
		}
	}
	if !response.OK {
		return errors.New(response.Message)
	}
	return nil
}

func enabledPlugins(all []pluginStatus) bool {
	for _, p := range all {
		if p.Installed && p.Enabled {
			return true
		}
	}
	return false
}

func localPluginStatuses(cfg appConfig, defs map[string]pluginDefinition) []pluginStatus {
	all := make(map[string]pluginDefinition, len(defs)+len(cfg.Features))
	for name, d := range defs {
		all[name] = d
	}
	for name, f := range cfg.Features {
		if _, ok := all[name]; !ok {
			all[name] = pluginDefinition{Installed: f.Enabled || f.Command != "", Feature: f}
		}
	}
	var out []pluginStatus
	for _, name := range pluginNames(all) {
		d := all[name]
		p := pluginStatus{Name: name, Kind: "companion", Installed: d.Installed, Enabled: d.Installed && d.Feature.Enabled, State: "available"}
		if name == "content" {
			p.Kind, p.Description = "builtin", "Scoped file content search"
		}
		if p.Installed {
			p.State = "pending_service"
			if !p.Enabled {
				p.State = "disabled"
			}
		}
		out = append(out, p)
	}
	return out
}

func filterPluginStatuses(all []pluginStatus, name string) []pluginStatus {
	for _, p := range all {
		if p.Name == name {
			return []pluginStatus{p}
		}
	}
	return nil
}
func pluginsReady(all []pluginStatus) bool {
	found := false
	for _, p := range all {
		if p.Installed && p.Enabled {
			found = true
			if p.State != "ready" || p.Error != "" {
				return false
			}
		}
	}
	return found
}

func waitForPlugins(pipe, path, name string, timeout time.Duration, out io.Writer, jsonOut bool) pluginCommandResponse {
	result := pluginCommandResponse{Config: path, Service: true}
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		resp, err := pluginServiceCall(pipe, serviceRequest{Command: "plugin-list"})
		if err != nil || !resp.OK {
			if err != nil {
				result.Message = err.Error()
			} else {
				result.Message = resp.Message
			}
			return result
		}
		result.Plugins = resp.Plugins
		if name != "" {
			result.Plugins = filterPluginStatuses(result.Plugins, name)
		}
		if pluginsReady(result.Plugins) {
			result.OK = true
			result.Message = "plugin ready"
			return result
		}
		for _, p := range result.Plugins {
			if p.Error != "" || p.State == "disabled" || p.State == "available" {
				result.Message = "plugin " + p.Name + ": " + p.State
				if p.Error != "" {
					result.Message += " — " + p.Error
				}
				return result
			}
		}
		if time.Now().After(deadline) {
			result.Message = "timed out waiting for plugin readiness; indexing continues in the background"
			return result
		}
		if !jsonOut {
			b, _ := json.Marshal(result.Plugins)
			if string(b) != last {
				for _, p := range result.Plugins {
					fmt.Fprintf(out, "%s: %s", p.Name, p.State)
					if p.Progress != nil && p.Progress.Total > 0 {
						fmt.Fprintf(out, " %d/%d", p.Progress.Current, p.Progress.Total)
					}
					fmt.Fprintln(out)
				}
				last = string(b)
			}
		}
		time.Sleep(min(time.Second, time.Until(deadline)))
	}
}
