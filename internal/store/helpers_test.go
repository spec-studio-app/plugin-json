package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/client"
	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// open opens a store on dir, as a relay host, and closes it at the end.
func open(t *testing.T, dir string) (*Store, *logBuffer) {
	t.Helper()
	s := New()
	log := &logBuffer{}
	err := s.Open(ctx(t), plugin.OpenOptions{
		Settings: map[string]string{SettingDataDir: dir},
		Host:     protocol.Host{Kind: "relay", Version: "0.6.0"},
		Log:      slog.New(slog.NewTextHandler(log, nil)),
	})
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, log
}

// serve serves a store on dir over an in-process stdio pipe, with the SDK,
// and does the handshake.
func serve(t *testing.T, dir string) (*client.Client, *logBuffer) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &logBuffer{}
	p := plugin.Plugin{
		Info:     protocol.PluginInfo{Name: "json", Version: "test", Repo: "https://github.com/spec-studio-app/plugin-json"},
		Settings: []protocol.Setting{{Name: SettingDataDir, Description: "The data folder."}},
		Store:    New(),
	}
	done := make(chan struct{})
	go func() {
		plugin.Run(context.Background(), p, plugin.Options{Stdin: inR, Stdout: outW, Stderr: stderr})
		outW.Close()
		close(done)
	}()
	c := client.Connect(outR, inW)
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	_, err := c.Handshake(ctx(t), protocol.HandshakeParams{
		Host:             protocol.Host{Kind: "relay", Version: "0.6.0"},
		ProtocolVersions: []string{"1.0"},
		Settings:         map[string]string{SettingDataDir: dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, stderr
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func str(s string) *string { return &s }

func put(t *testing.T, s *Store, collection, key string, value any, attrs map[string]any, expected *string) protocol.StatePutResult {
	t.Helper()
	r, err := s.Put(ctx(t), protocol.StatePutParams{Collection: collection, Key: key, Value: raw(value), Attributes: attrs, ExpectedVersion: expected})
	if err != nil {
		t.Fatalf("put %s/%s: %v", collection, key, err)
	}
	return r
}
