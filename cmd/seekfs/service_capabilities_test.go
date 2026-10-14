package main

import "testing"

func TestClassifyServiceCommand(t *testing.T) {
	tests := []struct {
		command string
		want    serviceCommandClass
	}{
		{"search", serviceCommandReadOnly},
		{"info", serviceCommandReadOnly},
		{"status", serviceCommandReadOnly},
		{"watch-delta", serviceCommandLocalOnly},
		{"open", serviceCommandLocalOnly},
		{"recent", serviceCommandLocalOnly},
		{"input-heartbeat", serviceCommandLocalOnly},
		{"index-usn", serviceCommandMutate},
		{"index-volumes", serviceCommandUnknown},
		{"service-index-usn", serviceCommandUnknown},
		{"", serviceCommandUnknown},
		{"reindex", serviceCommandUnknown},
	}
	for _, tt := range tests {
		if got := classifyServiceCommand(tt.command); got != tt.want {
			t.Errorf("classifyServiceCommand(%q) = %v, want %v", tt.command, got, tt.want)
		}
	}
}

func TestServiceCommandAllowed(t *testing.T) {
	tests := []struct {
		command string
		caps    serviceCapabilities
		want    bool
	}{
		// Ordinary local user: read-only allowed, mutation denied.
		{"search", serviceCapabilities{ReadOnly: true}, true},
		{"info", serviceCapabilities{ReadOnly: true}, true},
		{"watch-delta", serviceCapabilities{ReadOnly: true}, true},
		{"open", serviceCapabilities{ReadOnly: true}, true},
		{"recent", serviceCapabilities{ReadOnly: true}, true},
		{"input-heartbeat", serviceCapabilities{ReadOnly: true}, true},
		{"index-usn", serviceCapabilities{ReadOnly: true}, false},
		// Elevated: mutation allowed.
		{"index-usn", serviceCapabilities{ReadOnly: true, Mutate: true}, true},
		{"search", serviceCapabilities{ReadOnly: true, Mutate: true}, true},
		// Deny by default: unknown commands are always denied.
		{"reindex", serviceCapabilities{ReadOnly: true, Mutate: true}, false},
		{"", serviceCapabilities{ReadOnly: true, Mutate: true}, false},
		// No capabilities at all: nothing allowed.
		{"search", serviceCapabilities{}, false},
		{"index-usn", serviceCapabilities{}, false},
		// Remote read-only callers get the explicit wire allowlist: search and
		// sanitized info only.  status is folded into info and is not a
		// standalone remote command; watch-delta is deferred until Phase 7.
		{"search", serviceCapabilities{ReadOnly: true, Remote: true}, true},
		{"info", serviceCapabilities{ReadOnly: true, Remote: true}, true},
		{"status", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"watch-delta", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"open", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"recent", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"input-heartbeat", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"index-usn", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		{"reindex", serviceCapabilities{ReadOnly: true, Remote: true}, false},
		// The remote allowlist is applied BEFORE capability-class dispatch: a
		// remote principal with Mutate enabled still cannot reach mutation
		// commands (or any command outside the search/info wire contract).
		{"search", serviceCapabilities{ReadOnly: true, Mutate: true, Remote: true}, true},
		{"info", serviceCapabilities{ReadOnly: true, Mutate: true, Remote: true}, true},
		{"index-usn", serviceCapabilities{ReadOnly: true, Mutate: true, Remote: true}, false},
		{"status", serviceCapabilities{ReadOnly: true, Mutate: true, Remote: true}, false},
	}
	for _, tt := range tests {
		if got := serviceCommandAllowed(tt.command, tt.caps); got != tt.want {
			t.Errorf("serviceCommandAllowed(%q, %+v) = %v, want %v", tt.command, tt.caps, got, tt.want)
		}
	}
}

func TestLocalServiceCapabilities(t *testing.T) {
	tests := []struct {
		name      string
		principal servicePrincipal
		wantMut   bool
		wantRO    bool
	}{
		{"ordinary user", servicePrincipal{Elevated: false}, false, true},
		{"elevated admin", servicePrincipal{Elevated: true}, true, true},
		{"system", servicePrincipal{Elevated: true}, true, true},
	}
	for _, tt := range tests {
		caps := localServiceCapabilities(tt.principal)
		if caps.ReadOnly != tt.wantRO {
			t.Errorf("%s: ReadOnly = %v, want %v", tt.name, caps.ReadOnly, tt.wantRO)
		}
		if caps.Mutate != tt.wantMut {
			t.Errorf("%s: Mutate = %v, want %v", tt.name, caps.Mutate, tt.wantMut)
		}
		if caps.Remote {
			t.Errorf("%s: Remote should be false for a local principal", tt.name)
		}
	}
}

func TestServicePrincipalFromTokenInvalidFailsClosed(t *testing.T) {
	// An invalid token handle yields no user SID and no elevation: read-only.
	p := servicePrincipalFromToken(0)
	if p.Elevated {
		t.Error("invalid token must not produce an elevated principal")
	}
	if p.SID != "" {
		t.Errorf("invalid token SID = %q, want empty", p.SID)
	}
}

func TestServiceFatalExitSeam(t *testing.T) {
	// The fatal-exit seam must be non-nil so the impersonation rollback path
	// can fail the process rather than returning a contaminated thread to Go.
	if serviceFatalExit == nil {
		t.Fatal("serviceFatalExit must be wired to a process exit hook")
	}
}
