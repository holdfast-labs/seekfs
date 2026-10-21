package main

import (
	"strings"
	"testing"
)

func TestDecideServiceStartup(t *testing.T) {
	tests := []struct {
		name       string
		in         serviceStartupInput
		wantAction serviceStartupAction
		wantMsg    string
	}{
		{
			name:       "no pipe no sibling refuses",
			in:         serviceStartupInput{},
			wantAction: serviceStartupRefuse,
			wantMsg:    "was not found next to",
		},
		{
			name:       "no pipe with sibling spawns",
			in:         serviceStartupInput{SiblingExists: true},
			wantAction: serviceStartupSpawn,
			wantMsg:    "Starting seekfs service",
		},
		{
			name:       "pipe timeout refuses",
			in:         serviceStartupInput{PipeTimeout: true, SiblingExists: true},
			wantAction: serviceStartupRefuse,
			wantMsg:    "busy",
		},
		{
			name:       "installed service with sibling connects",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityInstalled, SiblingExists: true},
			wantAction: serviceStartupConnect,
		},
		{
			name:       "installed service without sibling connects never spawns",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityInstalled},
			wantAction: serviceStartupConnect,
		},
		{
			name:       "standalone sibling with sibling connects",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityStandaloneSibling, SiblingExists: true},
			wantAction: serviceStartupConnect,
		},
		{
			name:       "standalone sibling without sibling connects",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityStandaloneSibling},
			wantAction: serviceStartupConnect,
		},
		{
			name:       "foreign process refuses",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityForeign, SiblingExists: true},
			wantAction: serviceStartupRefuse,
			wantMsg:    "Another process",
		},
		{
			name:       "stale identity refuses",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityStale, SiblingExists: true},
			wantAction: serviceStartupRefuse,
			wantMsg:    "stale identity",
		},
		{
			name:       "unknown identity refuses",
			in:         serviceStartupInput{PipeResponds: true, Identity: serviceIdentityNone, SiblingExists: true},
			wantAction: serviceStartupRefuse,
			wantMsg:    "did not report a service identity",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideServiceStartup(tt.in)
			if got.Action != tt.wantAction {
				t.Fatalf("action = %v, want %v", got.Action, tt.wantAction)
			}
			if tt.wantMsg != "" && !strings.Contains(got.Message, tt.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", got.Message, tt.wantMsg)
			}
		})
	}
}

func identityFixture() (serviceIdentityExpectation, serviceResponse) {
	exp := serviceIdentityExpectation{
		Executable:     `C:\apps\seekfs\seekfs.exe`,
		ExecutableHash: "hash-1",
		Version:        "1.12.0",
		Commit:         "abc1234",
		Date:           "2026-10-19T00:00:00Z",
		BuildFlavor:    nativeServiceBuildFlavor,
		PipeName:       `\\.\pipe\seekfs-service`,
	}
	resp := serviceResponse{
		OK:             true,
		PID:            4242,
		Executable:     exp.Executable,
		ExecutableHash: exp.ExecutableHash,
		Version:        exp.Version,
		Commit:         exp.Commit,
		Date:           exp.Date,
		BuildFlavor:    exp.BuildFlavor,
		PipeName:       exp.PipeName,
		ProcessMode:    "standalone",
	}
	return exp, resp
}

func TestClassifyServiceIdentity(t *testing.T) {
	exp, resp := identityFixture()

	t.Run("standalone sibling", func(t *testing.T) {
		if got := classifyServiceIdentity(resp, exp); got != serviceIdentityStandaloneSibling {
			t.Fatalf("identity = %v, want standalone sibling", got)
		}
	})
	t.Run("installed service", func(t *testing.T) {
		installed := resp
		installed.ProcessMode = "windows-service"
		installed.Executable = `C:\Program Files\seekfs\seekfs.exe`
		installed.ExecutableHash = "other-install-hash"
		if got := classifyServiceIdentity(installed, exp); got != serviceIdentityInstalled {
			t.Fatalf("identity = %v, want installed", got)
		}
	})
	t.Run("stale build", func(t *testing.T) {
		stale := resp
		stale.Commit = "oldcomm"
		if got := classifyServiceIdentity(stale, exp); got != serviceIdentityStale {
			t.Fatalf("identity = %v, want stale", got)
		}
	})
	t.Run("stale hash", func(t *testing.T) {
		stale := resp
		stale.ExecutableHash = "hash-0"
		if got := classifyServiceIdentity(stale, exp); got != serviceIdentityStale {
			t.Fatalf("identity = %v, want stale", got)
		}
	})
	t.Run("foreign executable", func(t *testing.T) {
		foreign := resp
		foreign.Executable = `C:\other\seekfs.exe`
		if got := classifyServiceIdentity(foreign, exp); got != serviceIdentityForeign {
			t.Fatalf("identity = %v, want foreign", got)
		}
	})
	t.Run("no pid is foreign", func(t *testing.T) {
		foreign := resp
		foreign.PID = 0
		if got := classifyServiceIdentity(foreign, exp); got != serviceIdentityForeign {
			t.Fatalf("identity = %v, want foreign", got)
		}
	})
	t.Run("unknown process mode is foreign", func(t *testing.T) {
		foreign := resp
		foreign.ProcessMode = ""
		if got := classifyServiceIdentity(foreign, exp); got != serviceIdentityForeign {
			t.Fatalf("identity = %v, want foreign", got)
		}
	})
}
