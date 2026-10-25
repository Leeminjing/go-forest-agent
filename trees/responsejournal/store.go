// Package responsejournal persists provider continuation separately from Agent
// checkpoints. It implements the contract owned by the Responses adapter.
package responsejournal

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
	"sort"
	"strings"

	"github.com/Leeminjing/go-forest-agent/trees/openairesponses"
	"github.com/Leeminjing/go-forest/forest"
)

type Store struct{ directory string }

func New(directory string) (*Store, error) {
	if directory == "" {
		return nil, openairesponses.ErrContinuation
	}
	path, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	return &Store{path}, nil
}
func (s *Store) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.directory, hex.EncodeToString(sum[:]))
}
func (s *Store) Load(ctx context.Context, id string) ([]openairesponses.Record, error) {
	if id == "" || len(id) > 256 {
		return nil, openairesponses.ErrContinuation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return []openairesponses.Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, openairesponses.ErrContinuation
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) > 128 {
		return nil, openairesponses.ErrContinuation
	}
	records := make([]openairesponses.Record, 0, len(names))
	total := 0
	for i, name := range names {
		if name != fmt.Sprintf("%020d.json", i+1) {
			return nil, openairesponses.ErrContinuation
		}
		file, err := os.Open(filepath.Join(s.path(id), name))
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(file, openairesponses.MaxRecordBytes+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(data) > openairesponses.MaxRecordBytes {
			return nil, openairesponses.ErrContinuation
		}
		var record openairesponses.Record
		d := json.NewDecoder(strings.NewReader(string(data)))
		d.DisallowUnknownFields()
		if d.Decode(&record) != nil || d.Decode(new(any)) != io.EOF {
			return nil, openairesponses.ErrContinuation
		}
		exact, marshalErr := json.Marshal(record)
		total += len(data)
		if marshalErr != nil || !bytes.Equal(data, exact) || total > 32<<20 {
			return nil, openairesponses.ErrContinuation
		}
		records = append(records, record)
	}
	if err := openairesponses.CheckChain(id, records); err != nil {
		return nil, err
	}
	return records, ctx.Err()
}
func (s *Store) Append(ctx context.Context, id string, expected uint64, record openairesponses.Record) error {
	records, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	if uint64(len(records)) != expected || record.Sequence != expected+1 {
		return openairesponses.ErrConflict
	}
	if err := openairesponses.CheckChain(id, append(records, record)); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > openairesponses.MaxRecordBytes {
		return openairesponses.ErrContinuation
	}
	total := len(data)
	for _, old := range records {
		previous, err := json.Marshal(old)
		if err != nil {
			return err
		}
		total += len(previous)
	}
	if total > 32<<20 {
		return openairesponses.ErrContinuation
	}
	directory := s.path(id)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Link(file.Name(), filepath.Join(directory, fmt.Sprintf("%020d.json", record.Sequence))); errors.Is(err, os.ErrExist) {
		return openairesponses.ErrConflict
	}
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
func Definition(directory string) forest.Definition {
	return forest.Definition{TypeID: "agent.openai.response-journal", Version: "1", Capabilities: []forest.CapabilityDescriptor{forest.Capability[openairesponses.Journal]("journal", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		s, err := New(directory)
		if err != nil {
			return nil, err
		}
		return forest.Hooks{}, forest.Provide(env, "journal", s)
	}}
}
