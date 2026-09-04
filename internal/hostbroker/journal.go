package hostbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

const (
	journalLimit      = 4 << 20
	maxJournalRecords = 8192
)

const (
	recordReceived  = "received"
	recordAccepted  = "accepted"
	recordTerminal  = "terminal"
	recordDelivered = "delivered"
)

type Protector interface {
	Seal([]byte) ([]byte, error)
	Open([]byte) ([]byte, error)
}

type TaskRecord struct {
	Launch       protocol.TaskLaunchMsg `json:"launch"`
	Child        protocol.AgentRef      `json:"child"`
	ChildSession string                 `json:"child_session"`
	ChildSecret  string                 `json:"child_secret"`
	State        string                 `json:"state"`
	Terminal     json.RawMessage        `json:"terminal,omitempty"`
}

type Journal interface {
	Get(string) (TaskRecord, bool)
	Put(TaskRecord) error
	Records() []TaskRecord
}

type FileJournal struct {
	mu        sync.Mutex
	path      string
	protector Protector
	records   map[string]TaskRecord
}

func NewFileJournal(path string, protector Protector) (*FileJournal, error) {
	if !filepath.IsAbs(path) || protector == nil {
		return nil, errors.New("journal requires an absolute path and protector")
	}
	journal := &FileJournal{path: path, protector: protector, records: make(map[string]TaskRecord)}
	sealed, err := readRegular(path, journalLimit)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := protector.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open host journal: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal.records); err != nil {
		return nil, fmt.Errorf("decode host journal: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("decode host journal: trailing data")
	}
	for id, record := range journal.records {
		if err := validateRecord(id, record); err != nil {
			return nil, fmt.Errorf("decode host journal: %w", err)
		}
	}
	if len(journal.records) > maxJournalRecords {
		return nil, errors.New("decode host journal: too many records")
	}
	return journal, nil
}

func (j *FileJournal) Get(taskID string) (TaskRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[taskID]
	return record, ok
}

func (j *FileJournal) Records() []TaskRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	ids := slices.Sorted(maps.Keys(j.records))
	result := make([]TaskRecord, 0, len(ids))
	for _, id := range ids {
		result = append(result, j.records[id])
	}
	return result
}

func (j *FileJournal) Put(record TaskRecord) error {
	if record.State == recordDelivered {
		record = TaskRecord{Launch: protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: record.Launch.TaskID}, State: recordDelivered}
	}
	if err := validateRecord(record.Launch.TaskID, record); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	previous, existed := j.records[record.Launch.TaskID]
	var evictedID string
	var evicted TaskRecord
	if !existed && len(j.records) >= maxJournalRecords {
		for _, id := range slices.Sorted(maps.Keys(j.records)) {
			if j.records[id].State == recordDelivered {
				evictedID, evicted = id, j.records[id]
				delete(j.records, id)
				break
			}
		}
		if evictedID == "" {
			return errors.New("host journal is full")
		}
	}
	j.records[record.Launch.TaskID] = record
	plain, err := json.Marshal(j.records)
	if err == nil {
		var sealed []byte
		sealed, err = j.protector.Seal(plain)
		if err == nil {
			err = writeProtected(j.path, sealed)
		}
	}
	if err != nil {
		if existed {
			j.records[record.Launch.TaskID] = previous
		} else {
			delete(j.records, record.Launch.TaskID)
		}
		if evictedID != "" {
			j.records[evictedID] = evicted
		}
		return fmt.Errorf("persist host journal: %w", err)
	}
	return nil
}

func validateRecord(id string, record TaskRecord) error {
	if id == "" || id != record.Launch.TaskID || record.State != recordReceived && record.State != recordAccepted && record.State != recordTerminal && record.State != recordDelivered {
		return errors.New("journal record has no task id or valid state")
	}
	if record.State == recordReceived && (record.ChildSession != "" || len(record.Terminal) != 0) ||
		record.State == recordAccepted && record.ChildSession == "" ||
		record.State == recordTerminal && len(record.Terminal) == 0 ||
		record.State == recordDelivered && (record.Launch.Type != protocol.TaskLaunch || record.ChildSession != "" || len(record.Terminal) != 0) ||
		(record.ChildSession == "") != (record.ChildSecret == "") {
		return errors.New("journal record state is inconsistent")
	}
	return nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("protected state must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("protected state exceeds size limit")
	}
	return data, nil
}

func writeProtected(path string, data []byte) error {
	if len(data) == 0 || len(data) > journalLimit {
		return errors.New("protected state is empty or oversized")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("protected state must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".host-state-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporary, path); err != nil {
		return err
	}
	keep = true
	return nil
}
