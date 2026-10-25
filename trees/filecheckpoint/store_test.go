package filecheckpoint_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/filecheckpoint"
)

func TestAtomicCASAcrossStoreInstancesAndCorruptLatest(t *testing.T) {
	directory := t.TempDir()
	ctx := context.Background()
	var won, conflicts atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := filecheckpoint.New(directory)
			if err != nil {
				t.Error(err)
				return
			}
			err = s.CompareAndSwap(ctx, "../run", 0, agent.Run{ID: "../run", Revision: 1, Status: agent.ModelReady})
			if err == nil {
				won.Add(1)
			} else if errors.Is(err, agent.ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || conflicts.Load() != 15 {
		t.Fatal(won.Load(), conflicts.Load())
	}
	s, err := filecheckpoint.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Load(ctx, "../run")
	if err != nil || run.Revision != 1 {
		t.Fatal(run, err)
	}
	run.Revision = 2
	if err := s.CompareAndSwap(ctx, run.ID, 1, run); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(run.ID))
	latest := filepath.Join(directory, hex.EncodeToString(sum[:]), "00000000000000000002.json")
	if err := os.WriteFile(latest, []byte(`{"id":"truncated`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, run.ID); !errors.Is(err, agent.ErrCorrupt) {
		t.Fatal("corruption silently rolled back", err)
	}
	if err := s.CompareAndSwap(ctx, run.ID, 2, agent.Run{ID: run.ID, Revision: 3}); !errors.Is(err, agent.ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestInvalidUTF8AndSemanticStateAreNotNormalizedOrResumed(t *testing.T) {
	directory := t.TempDir()
	s, err := filecheckpoint.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	call := agent.ToolCall{ID: "same", Name: "add", Arguments: json.RawMessage(`{}`)}
	spec := agent.ToolSpec{Name: "add", Version: "1", Parameters: json.RawMessage(`{}`)}
	for _, run := range []agent.Run{
		{ID: "duplicate", Revision: 1, Status: agent.ToolsReady, ModelTurns: 1, Messages: []agent.Message{{Role: "assistant", Calls: []agent.ToolCall{call, call}}}, Calls: []agent.PendingCall{{Call: call, Tool: spec}, {Call: call, Tool: spec}}},
		{ID: "missing-plan", Revision: 1, Status: agent.ModelReady, ModelTurns: 1, Messages: []agent.Message{{Role: "assistant", Calls: []agent.ToolCall{call}}, {Role: "tool", CallID: "same", Content: "known"}}},
		{ID: "counter", Revision: 1, Status: agent.ModelReady, ModelTurns: 2, Messages: []agent.Message{{Role: "user", Content: "prompt"}}},
		{ID: "invalid-source", Revision: 1, Status: agent.ModelReady, Messages: []agent.Message{{Role: "user", Content: string([]byte{0xff})}}},
	} {
		if err := s.CompareAndSwap(context.Background(), run.ID, 0, run); !errors.Is(err, agent.ErrCorrupt) {
			t.Fatal(run.ID, err)
		}
	}
	run := agent.Run{ID: "raw-corruption", Revision: 1, Status: agent.ModelReady, Messages: []agent.Message{{Role: "user", Content: "marker"}}}
	if err := s.CompareAndSwap(context.Background(), run.ID, 0, run); err != nil {
		t.Fatal(err)
	}
	// Locate the one published record without relying on the private ID codec.
	var file string
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".json" {
			file = path
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("marker"), []byte{0xff}, 1)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background(), run.ID); !errors.Is(err, agent.ErrCorrupt) {
		t.Fatal("invalid UTF-8 silently changed", err)
	}
}

func TestDetachedDataAndCancellation(t *testing.T) {
	s, err := filecheckpoint.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := agent.Run{ID: "x", Revision: 1, Status: agent.ModelReady, Messages: []agent.Message{{Role: "user", Content: "original"}}}
	if err := s.CompareAndSwap(context.Background(), "x", 0, run); err != nil {
		t.Fatal(err)
	}
	run.Messages[0].Content = "mutated"
	loaded, err := s.Load(context.Background(), "x")
	if err != nil || loaded.Messages[0].Content != "original" {
		t.Fatal(loaded, err)
	}
	loaded.Messages[0].Content = "again"
	loaded, err = s.Load(context.Background(), "x")
	if err != nil || loaded.Messages[0].Content != "original" {
		t.Fatal(loaded, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Load(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.CompareAndSwap(ctx, "x", 1, agent.Run{ID: "x", Revision: 2}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
