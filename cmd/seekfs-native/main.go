package main

import (
	"flag"
	"fmt"
	"os"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "seekfs-native:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("seekfs-native", flag.ContinueOnError)
	pipe := flags.String("pipe", defaultServicePipe, "service named pipe")
	limit := flags.Int("n", defaultResultLimit, "default result limit")
	showVersion := flags.Bool("version", false, "print version and exit")
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
	return runUI(*pipe, *limit)
}
