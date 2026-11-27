package main

import (
	"testing"

	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// R1.S5: the plugin's own definition gives state, blobs and watch, no events,
// the data_dir setting, and durable.
func TestDescribe(t *testing.T) {
	p := newPlugin()
	caps := plugin.Capabilities(p.Store)
	want := []protocol.Capability{protocol.CapState, protocol.CapBlobs, protocol.CapWatch}
	if len(caps) != len(want) {
		t.Fatalf("capabilities %v, want %v", caps, want)
	}
	for i := range want {
		if caps[i] != want[i] {
			t.Fatalf("capabilities %v, want %v", caps, want)
		}
	}
	if len(p.Settings) != 1 || p.Settings[0].Name != "data_dir" || p.Settings[0].Required || p.Settings[0].Secret {
		t.Fatalf("settings %+v", p.Settings)
	}
	d, ok := p.Store.(plugin.Durable)
	if !ok || !d.Durable() {
		t.Fatal("the store is not durable")
	}
	if p.Info.Name != "json" || p.Info.Repo != Repo {
		t.Fatalf("info %+v", p.Info)
	}
}
