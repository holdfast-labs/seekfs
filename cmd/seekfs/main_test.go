package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Service-build tests write small sidecars into temp dirs; the production
	// free-space reserve would fail them on machines with little free disk.
	// Tests that exercise the reserve set it explicitly.
	contentDiskReserveBytes = 0
	os.Exit(m.Run())
}
