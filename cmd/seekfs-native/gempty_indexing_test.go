package main

import "testing"

func TestIndexingSummary(t *testing.T) {
	if title, ok := indexingSummary([]DBVolume{{Volume: "C:", State: "ready"}}); ok || title != "" {
		t.Fatalf("ready volume = %q, %v", title, ok)
	}
	one := []DBVolume{{Volume: "X:", State: "indexing", Progress: &serviceVolumeProgress{Done: 42, Total: 100}}}
	if title, ok := indexingSummary(one); !ok || title != "Indexing X: 42%" {
		t.Fatalf("one volume = %q, %v", title, ok)
	}
	unknown := []DBVolume{{Volume: "X:", State: "indexing"}}
	if title, ok := indexingSummary(unknown); !ok || title != "Indexing X:\u2026" {
		t.Fatalf("unknown total = %q, %v", title, ok)
	}
	two := []DBVolume{{Volume: "C:", State: "indexing"}, {Volume: "D:", State: "indexing"}, {Volume: "E:", State: "ready"}}
	if title, ok := indexingSummary(two); !ok || title != "Indexing C:, D:\u2026" {
		t.Fatalf("two volumes = %q, %v", title, ok)
	}
}
