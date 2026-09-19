package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RecentRequestRecord is one durable request outcome used to rehydrate the
// dashboard's per-credential rolling history after process restart.
type RecentRequestRecord struct {
	AuthID   string    `json:"auth_id"`
	Provider string    `json:"provider,omitempty"`
	Time     time.Time `json:"time"`
	Success  bool      `json:"success"`
}

// RecentRequestStore is an append-only JSONL store for dashboard traffic.
type RecentRequestStore struct {
	mu   sync.Mutex
	path string
}

func NewRecentRequestStore(path string) *RecentRequestStore {
	return &RecentRequestStore{path: strings.TrimSpace(path)}
}

func (s *RecentRequestStore) Append(record RecentRequestRecord) error {
	if s == nil || s.path == "" || strings.TrimSpace(record.AuthID) == "" || record.Time.IsZero() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o700); errMkdir != nil {
		return fmt.Errorf("create recent request directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open recent request history: %w", errOpen)
	}
	encoded, errMarshal := json.Marshal(&record)
	if errMarshal == nil {
		_, errMarshal = file.Write(append(encoded, '\n'))
	}
	if errClose := file.Close(); errMarshal == nil && errClose != nil {
		errMarshal = errClose
	}
	if errMarshal != nil {
		return fmt.Errorf("append recent request history: %w", errMarshal)
	}
	return nil
}

func (s *RecentRequestStore) Load() ([]RecentRequestRecord, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, errOpen := os.Open(s.path)
	if errors.Is(errOpen, os.ErrNotExist) {
		return nil, nil
	}
	if errOpen != nil {
		return nil, fmt.Errorf("open recent request history: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	var records []RecentRequestRecord
	reader := bufio.NewReader(file)
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			record, errDecode := decodeRecentRequestRecord(line)
			if errDecode != nil {
				if errors.Is(errRead, io.EOF) {
					break
				}
				return nil, fmt.Errorf("decode recent request history: %w", errDecode)
			}
			if strings.TrimSpace(record.AuthID) != "" && !record.Time.IsZero() {
				records = append(records, record)
			}
		}
		if errors.Is(errRead, io.EOF) {
			break
		}
		if errRead != nil {
			return nil, fmt.Errorf("read recent request history: %w", errRead)
		}
	}
	return records, nil
}

func decodeRecentRequestRecord(line []byte) (RecentRequestRecord, error) {
	trimmed := bytes.TrimSpace(line)
	var record RecentRequestRecord
	if errDecode := json.Unmarshal(trimmed, &record); errDecode == nil && !record.Time.IsZero() {
		return record, nil
	}

	// Backward compatibility for the original local dashboard history format.
	var legacy struct {
		AuthID   string `json:"auth_id"`
		Provider string `json:"provider"`
		Unix     int64  `json:"unix"`
		Success  bool   `json:"success"`
	}
	if errDecode := json.Unmarshal(trimmed, &legacy); errDecode != nil {
		return RecentRequestRecord{}, errDecode
	}
	if legacy.Unix <= 0 {
		return RecentRequestRecord{}, errors.New("missing request timestamp")
	}
	return RecentRequestRecord{
		AuthID:   legacy.AuthID,
		Provider: legacy.Provider,
		Time:     time.Unix(legacy.Unix, 0),
		Success:  legacy.Success,
	}, nil
}
