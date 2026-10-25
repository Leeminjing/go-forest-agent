// Package filecheckpoint appends immutable revisions on a local filesystem.
// Publication requires atomic hard links; it never falls back to overwrite.
package filecheckpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

const maxBytes = 32 << 20

type Store struct{ directory string }

func New(directory string) (*Store, error) {
	if directory == "" {
		return nil, agent.ErrInvalid
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	return &Store{directory: absolute}, nil
}
func (s *Store) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.directory, hex.EncodeToString(sum[:]))
}
func (s *Store) Load(ctx context.Context, id string) (agent.Run, error) {
	if err := ctx.Err(); err != nil {
		return agent.Run{}, err
	}
	if id == "" || len(id) > 256 {
		return agent.Run{}, agent.ErrInvalid
	}
	entries, err := os.ReadDir(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return agent.Run{}, agent.ErrNotFound
	}
	if err != nil {
		return agent.Run{}, err
	}
	var latest uint64
	var name string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || n == 0 || fmt.Sprintf("%020d.json", n) != entry.Name() {
			return agent.Run{}, agent.ErrCorrupt
		}
		if n > latest {
			latest = n
			name = entry.Name()
		}
	}
	if latest == 0 {
		return agent.Run{}, agent.ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.path(id), name))
	if err != nil {
		return agent.Run{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return agent.Run{}, err
	}
	if len(data) > maxBytes {
		return agent.Run{}, agent.ErrCorrupt
	}
	var run agent.Run
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&run) != nil || d.Decode(new(any)) != io.EOF || run.ID != id || run.Revision != latest || run.Delivered > uint64(len(run.Events)) || run.Current < 0 || run.Current > len(run.Calls) {
		return agent.Run{}, agent.ErrCorrupt
	}
	for i, event := range run.Events {
		if event.RunID != id || event.Sequence != uint64(i+1) || event.Temporary {
			return agent.Run{}, agent.ErrCorrupt
		}
	}
	if err := run.Validate(); err != nil {
		return agent.Run{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.Run{}, err
	}
	return run, nil
}
func (s *Store) CompareAndSwap(ctx context.Context, id string, expected uint64, next agent.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || len(id) > 256 || next.ID != id || expected == ^uint64(0) || next.Revision != expected+1 {
		return agent.ErrInvalid
	}
	if err := next.Validate(); err != nil {
		return err
	}
	previous, err := s.Load(ctx, id)
	if errors.Is(err, agent.ErrNotFound) {
		if expected != 0 {
			return agent.ErrConflict
		}
	} else if err != nil {
		return err
	} else if previous.Revision != expected {
		return agent.ErrConflict
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return agent.ErrInvalid
	}
	directory := s.path(id)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(directory, ".pending-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(name, filepath.Join(directory, fmt.Sprintf("%020d.json", next.Revision))); err != nil {
		if errors.Is(err, os.ErrExist) {
			return agent.ErrConflict
		}
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := dir.Sync(); err != nil {
			return err
		}
	}
	return nil
}
func Definition(directory string) forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.checkpoint.file", Version: "1", Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Checkpoint]("checkpoint", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		store, err := New(directory)
		if err != nil {
			return nil, err
		}
		return forest.Hooks{}, forest.Provide(env, "checkpoint", store)
	}}
}
