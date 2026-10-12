//go:build windows

package main

import (
	"fmt"
	"os"
	"time"
)

// dispInfoLogCount bounds temporary LVN_GETDISPINFO tracing.
var dispInfoLogCount int

// heartbeat appends a timestamped line to a debug log. Temporary startup
// diagnostics for a hung-UI-thread investigation; removed before release.
func heartbeat(msg string) {
	f, err := os.OpenFile(`E:\bench\native-shots\uiloop.log`, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), msg)
}
