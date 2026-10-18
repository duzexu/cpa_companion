package companion

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

const maxUsageJournalEntry = 64 << 10
const rotateUsageJournalAt = 8 << 20

type usageJournalEntry struct {
	PlanID     string `json:"plan_id"`
	Charge     int64  `json:"charge"`
	MeterError string `json:"meter_error,omitempty"`
}

func (s *store) usageJournalPath() string {
	return s.usageJournalPathFor(s.db.UsageJournalGeneration)
}

func (s *store) usageJournalPathFor(generation int64) string {
	name := "usage.journal"
	if generation > 0 {
		name = "usage." + strconv.FormatInt(generation, 10) + ".journal"
	}
	return filepath.Join(filepath.Dir(s.path), name)
}

// A crash after creating the next journal but before installing its snapshot
// leaves an empty, uncommitted generation. This service uses one writer.
func (s *store) removeUncommittedJournal() error {
	if s.db.UsageJournalGeneration == math.MaxInt64 {
		return nil
	}
	err := os.Remove(s.usageJournalPathFor(s.db.UsageJournalGeneration + 1))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// replayUsageJournal applies only entries newer than the last JSON snapshot.
// A torn final append is discarded; complete but unsnapshotted entries remain
// billable after an unclean shutdown.
func (s *store) replayUsageJournal() error {
	if s.db.UsageJournalGeneration < 0 {
		return errors.New("invalid usage journal generation")
	}
	f, err := os.OpenFile(s.usageJournalPath(), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) && s.db.UsageJournalOffset == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	offset := s.db.UsageJournalOffset
	if offset < 0 || offset > info.Size() {
		return errors.New("usage journal checkpoint is outside the journal")
	}
	if offset > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], offset-1); err != nil || last[0] != '\n' {
			return errors.New("usage journal checkpoint is not on a record boundary")
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr == io.EOF {
			if len(line) > 0 {
				if err := f.Truncate(offset); err != nil {
					return err
				}
				if err := f.Sync(); err != nil {
					return err
				}
			}
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if len(line) > maxUsageJournalEntry {
			return errors.New("usage journal entry exceeds maximum size")
		}
		var entry usageJournalEntry
		if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &entry); err != nil {
			return fmt.Errorf("decode usage journal at offset %d: %w", offset, err)
		}
		if entry.PlanID == "" {
			return fmt.Errorf("invalid usage journal entry at offset %d", offset)
		}
		if plan, ok := s.db.Plans[entry.PlanID]; ok {
			if entry.Charge > 0 && plan.UsedPicoUSD > math.MaxInt64-entry.Charge ||
				entry.Charge < 0 && (entry.Charge == math.MinInt64 || plan.UsedPicoUSD < -entry.Charge) {
				return errors.New("usage journal amount overflow")
			}
			plan.UsedPicoUSD += entry.Charge
			if entry.MeterError != "" {
				plan.UsageError = entry.MeterError
			}
			s.db.Plans[entry.PlanID] = plan
		}
		offset += int64(len(line))
		s.db.UsageJournalOffset = offset
	}
}

// The old journal stays valid until the new-generation snapshot is installed.
// The caller holds usageMu; this full snapshot occurs only after 8 MiB of
// incremental charges, not once per API request.
func (s *store) rotateUsageJournal(end int64) error {
	if end < rotateUsageJournalAt {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldGeneration, oldOffset := s.db.UsageJournalGeneration, s.db.UsageJournalOffset
	if oldGeneration == math.MaxInt64 {
		return errors.New("usage journal generation overflow")
	}
	newPath := s.usageJournalPathFor(oldGeneration + 1)
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(newPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(newPath)
		return err
	}
	dir, err := os.Open(filepath.Dir(newPath))
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	if err != nil {
		os.Remove(newPath)
		return err
	}
	s.db.UsageJournalGeneration, s.db.UsageJournalOffset = oldGeneration+1, 0
	if err := s.persistLocked(); err != nil {
		s.db.UsageJournalGeneration, s.db.UsageJournalOffset = oldGeneration, oldOffset
		os.Remove(newPath)
		return err
	}
	// The replacement snapshot's directory entry must be durable before the
	// old journal can be removed. On failure retain both generations: either
	// snapshot version can then recover after a power loss.
	dir, err = os.Open(filepath.Dir(s.path))
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	if err != nil {
		return err
	}
	return os.Remove(s.usageJournalPathFor(oldGeneration))
}

// appendUsageJournal is serialized by usageMu and performs no I/O under the
// store-wide lock. The entry is fsynced before the caller updates memory.
func (s *store) appendUsageJournal(entry usageJournalEntry) (int64, error) {
	b, err := json.Marshal(entry)
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')
	if len(b) > maxUsageJournalEntry {
		return 0, errors.New("usage journal entry exceeds maximum size")
	}
	path := s.usageJournalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return 0, err
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return 0, statErr
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	start := info.Size()
	if _, err := f.Write(b); err == nil {
		err = f.Sync()
	}
	if err == nil && created {
		var dir *os.File
		dir, err = os.Open(filepath.Dir(path))
		if err == nil {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		if rollbackErr := f.Truncate(start); rollbackErr != nil {
			return 0, errors.Join(err, rollbackErr)
		}
		if rollbackErr := f.Sync(); rollbackErr != nil {
			return 0, errors.Join(err, rollbackErr)
		}
		return 0, err
	}
	return start + int64(len(b)), nil
}
