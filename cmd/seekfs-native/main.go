package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"strconv"
	"strings"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "seekfs-native:", err)
		// The GUI subsystem build has no console; mirror fatal errors
		// where screenshots/debugging can find them.
		if f, ferr := os.OpenFile(`E:\bench\native-shots\fatal.log`, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); ferr == nil {
			fmt.Fprintln(f, "seekfs-native:", err)
			f.Close()
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("seekfs-native", flag.ContinueOnError)
	pipe := flags.String("pipe", defaultServicePipe, "service named pipe")
	limit := flags.Int("n", defaultResultLimit, "default result limit")
	showVersion := flags.Bool("version", false, "print version and exit")
	snapshot := flags.String("snapshot", "", "render a headless PNG snapshot to this path and exit")
	theme := flags.String("theme", "", "snapshot theme: dark|light (default follows the OS)")
	scale := flags.Float64("scale", 1, "snapshot dp scale (1 = 100%, 1.5 = 150%)")
	sizeFlag := flags.String("size", "1280x800", "snapshot size in device pixels (WxH)")
	query := flags.String("query", "", "snapshot startup query")
	viewFlag := flags.String("view", "", "snapshot view: details|icons|grouped")
	contentsFlag := flags.Bool("contents", false, "snapshot with Contents mode on")
	previewFlag := flags.Bool("preview", false, "snapshot with preview pane open")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("seekfs-native %s (%s %s)\n", version, commit, date)
		return nil
	}
	if *limit <= 0 {
		*limit = defaultResultLimit
	}
	if *snapshot != "" {
		size := image.Pt(1280, 800)
		if parts := strings.SplitN(*sizeFlag, "x", 2); len(parts) == 2 {
			if w, err := strconv.Atoi(parts[0]); err == nil && w > 0 {
				size.X = w
			}
			if h, err := strconv.Atoi(parts[1]); err == nil && h > 0 {
				size.Y = h
			}
		}
		return runSnapshot(*pipe, *limit, snapshotConfig{
			out: *snapshot, theme: *theme, scale: *scale, size: size, query: *query,
			view: *viewFlag, contents: *contentsFlag, preview: *previewFlag,
		})
	}
	return runUI(*pipe, *limit)
}
