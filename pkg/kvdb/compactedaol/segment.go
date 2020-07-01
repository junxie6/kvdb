package compactedaol

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/olif/kvdb/pkg/kvdb"
	"github.com/olif/kvdb/pkg/kvdb/record"
)

type segment struct {
	storagePath   string
	maxRecordSize int
	logger        *log.Logger
	async         bool
	suffix        string

	index      *index
	writeMutex *sync.Mutex
}

type index struct {
	mutex  sync.RWMutex
	table  map[string]int64
	cursor int64
}

// not safe for concurrent use, must be handled by consumer
func newSegment(baseDir string, maxRecordSize int, async bool, logger *log.Logger) segment {
	filename := genFileName(openSegmentSuffix)
	filePath := path.Join(baseDir, filename)

	return segment{
		storagePath: filePath,
		logger:      logger,
		async:       async,
		index: &index{
			cursor: 0,
			table:  map[string]int64{},
		},
	}
}

func fromFile(filePath string, maxRecordSize int, async bool, logger *log.Logger) (segment, error) {
	idx := index{
		cursor: 0,
		table:  map[string]int64{},
	}

	f, err := os.OpenFile(filePath, os.O_RDONLY|os.O_CREATE, 0600)
	defer f.Close()
	if err != nil {
		return segment{}, err
	}

	scanner, err := record.NewScanner(f, maxRecordSize)
	if err != nil {
		return segment{}, err
	}

	for scanner.Scan() {
		record := scanner.Record()
		idx.put(record.Key(), int64(record.Size()))
	}

	if scanner.Err() != nil {
		return segment{}, fmt.Errorf("could not scan entry, %w", err)
	}

	return segment{
		storagePath: filePath,
		index:       &idx,
		writeMutex:  &sync.Mutex{},
	}, nil
}

func genFileName(suffix string) string {
	return fmt.Sprintf("%d%s", time.Now().UTC().UnixNano(), suffix)
}

func (i *index) get(key string) (int64, bool) {
	i.mutex.RLock()
	defer i.mutex.RUnlock()
	val, ok := i.table[key]
	return val, ok
}

func (i *index) put(key string, written int64) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.table[key] = i.cursor
	i.cursor += written
}

func (segment segment) get(key string) ([]byte, error) {
	offset, ok := segment.index.get(key)
	if !ok {
		return nil, kvdb.NewNotFoundError(key)
	}

	f, err := os.OpenFile(segment.storagePath, os.O_CREATE|os.O_RDONLY, 0600)
	defer f.Close()
	if err != nil {
		return nil, err
	}

	_, err = f.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, err
	}

	scanner, err := record.NewScanner(f, segment.maxRecordSize)
	if err != nil {
		return nil, err
	}

	if scanner.Scan() {
		record := scanner.Record()

		if record.IsTombstone() {
			return nil, kvdb.NewNotFoundError(key)
		}

		return record.Value(), nil
	}

	return nil, kvdb.NewNotFoundError(key)
}

func (segment segment) append(record *record.Record) error {
	file, err := os.OpenFile(segment.storagePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	defer file.Close()
	if err != nil {
		return fmt.Errorf("could not open file: %s for write, %w", segment.storagePath, err)
	}

	n, err := record.Write(file)
	if err != nil {
		return fmt.Errorf("could not write record to file: %s, %w", segment.storagePath, err)
	}

	if !segment.async {
		if err := file.Sync(); err != nil {
			return err
		}
	}

	if err := file.Close(); err != nil {
		return err
	}

	segment.index.put(record.Key(), int64(n))
	return nil
}

func (s segment) changeSuffix(oldSuffix, newSuffix string) (segment, error) {
	newFilePath := strings.Replace(s.storagePath, oldSuffix, newSuffix, 1)

	if err := os.Rename(s.storagePath, newFilePath); err != nil {
		return segment{}, err
	}

	return segment{
		storagePath: newFilePath,
		logger:      s.logger,
		async:       s.async,
		index: &index{
			cursor: 0,
			table:  map[string]int64{},
		},
	}, nil
}

func (segment segment) size() int64 {
	return segment.index.cursor
}