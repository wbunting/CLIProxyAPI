package redisqueue

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const persistenceRewriteInterval = time.Minute

type persistedQueueItem struct {
	EnqueuedAt time.Time `json:"enqueued_at"`
	Payload    string    `json:"payload"`
}

// SetPersistencePath configures the durable usage queue file. Existing retained
// records are loaded immediately. An empty path disables persistence.
func SetPersistencePath(path string) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	global.persistencePath = strings.TrimSpace(path)
	global.items = nil
	global.head = 0
	global.lastPersistenceRewrite = time.Time{}
	if global.persistencePath == "" {
		return nil
	}
	if errLoad := global.loadPersistenceLocked(); errLoad != nil {
		return errLoad
	}
	return global.rewritePersistenceLocked(time.Now())
}

func (q *queue) reloadPersistenceIfEmpty() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.persistencePath == "" || q.head < len(q.items) {
		return nil
	}
	return q.loadPersistenceLocked()
}

func (q *queue) loadPersistenceLocked() error {
	if q.persistencePath == "" {
		return nil
	}
	file, errOpen := os.Open(q.persistencePath)
	if errors.Is(errOpen, os.ErrNotExist) {
		return nil
	}
	if errOpen != nil {
		return fmt.Errorf("open usage persistence: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	now := time.Now()
	loaded := make([]queueItem, 0)
	reader := bufio.NewReader(file)
	for {
		line, errRead := reader.ReadBytes('\n')
		completeLine := errRead == nil || errors.Is(errRead, io.EOF)
		if completeLine && len(bytes.TrimSpace(line)) > 0 {
			item, errDecode := decodePersistedQueueItem(line)
			if errDecode != nil {
				if errors.Is(errRead, io.EOF) {
					// Ignore an incomplete/corrupt final append. Earlier complete records
					// remain authoritative and the next rewrite repairs the tail.
					break
				}
				return fmt.Errorf("decode usage persistence: %w", errDecode)
			}
			loaded = append(loaded, item)
		}
		if errors.Is(errRead, io.EOF) {
			break
		}
		if errRead != nil {
			return fmt.Errorf("read usage persistence: %w", errRead)
		}
	}

	q.items = loaded
	q.head = 0
	q.pruneLocked(now)
	q.maybeCompactLocked()
	return nil
}

func decodePersistedQueueItem(line []byte) (queueItem, error) {
	var persisted persistedQueueItem
	if errUnmarshal := json.Unmarshal(bytes.TrimSpace(line), &persisted); errUnmarshal != nil {
		return queueItem{}, errUnmarshal
	}
	if persisted.EnqueuedAt.IsZero() {
		return queueItem{}, errors.New("missing enqueued_at")
	}
	payload, errDecode := base64.StdEncoding.DecodeString(persisted.Payload)
	if errDecode != nil {
		return queueItem{}, fmt.Errorf("decode payload: %w", errDecode)
	}
	if len(payload) == 0 {
		return queueItem{}, errors.New("empty payload")
	}
	return queueItem{enqueuedAt: persisted.EnqueuedAt, payload: payload}, nil
}

func (q *queue) appendPersistenceLocked(item queueItem) error {
	if q.persistencePath == "" {
		return nil
	}
	if errMkdir := os.MkdirAll(filepath.Dir(q.persistencePath), 0o700); errMkdir != nil {
		return fmt.Errorf("create usage persistence directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(q.persistencePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open usage persistence append: %w", errOpen)
	}
	persisted := persistedQueueItem{
		EnqueuedAt: item.enqueuedAt,
		Payload:    base64.StdEncoding.EncodeToString(item.payload),
	}
	encoded, errMarshal := json.Marshal(&persisted)
	if errMarshal == nil {
		encoded = append(encoded, '\n')
		_, errMarshal = file.Write(encoded)
	}
	if errClose := file.Close(); errMarshal == nil && errClose != nil {
		errMarshal = errClose
	}
	if errMarshal != nil {
		return fmt.Errorf("append usage persistence: %w", errMarshal)
	}
	return nil
}

func (q *queue) rewritePersistenceLocked(now time.Time) error {
	if q.persistencePath == "" {
		return nil
	}
	if errMkdir := os.MkdirAll(filepath.Dir(q.persistencePath), 0o700); errMkdir != nil {
		return fmt.Errorf("create usage persistence directory: %w", errMkdir)
	}
	tmpFile, errCreate := os.CreateTemp(filepath.Dir(q.persistencePath), ".usage-queue-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create usage persistence temp file: %w", errCreate)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	writer := bufio.NewWriter(tmpFile)
	var errWrite error
	for _, item := range q.items[q.head:] {
		persisted := persistedQueueItem{
			EnqueuedAt: item.enqueuedAt,
			Payload:    base64.StdEncoding.EncodeToString(item.payload),
		}
		var encoded []byte
		encoded, errWrite = json.Marshal(&persisted)
		if errWrite != nil {
			break
		}
		if _, errWrite = writer.Write(append(encoded, '\n')); errWrite != nil {
			break
		}
	}
	if errWrite == nil {
		errWrite = writer.Flush()
	}
	if errWrite == nil {
		errWrite = tmpFile.Sync()
	}
	if errClose := tmpFile.Close(); errWrite == nil && errClose != nil {
		errWrite = errClose
	}
	if errWrite != nil {
		cleanup()
		return fmt.Errorf("write usage persistence: %w", errWrite)
	}
	if errChmod := os.Chmod(tmpPath, 0o600); errChmod != nil {
		cleanup()
		return fmt.Errorf("chmod usage persistence: %w", errChmod)
	}
	if errRename := os.Rename(tmpPath, q.persistencePath); errRename != nil {
		cleanup()
		return fmt.Errorf("replace usage persistence: %w", errRename)
	}
	q.lastPersistenceRewrite = now
	return nil
}

func (q *queue) pruneAndPersist(now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked(now)
	q.maybeCompactLocked()
	_ = q.rewritePersistenceLocked(now)
}
