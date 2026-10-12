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
