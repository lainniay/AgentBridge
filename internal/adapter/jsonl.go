package adapter

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// ReadJSONL streams even very large lines. Historical readers can attempt to
// decode the last line if the producer omitted the trailing newline.
func ReadJSONL(path string, onLine func([]byte, int64) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if visitErr := onLine(line, offset); visitErr != nil {
				return visitErr
			}
			offset += int64(len(line))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// TailJSONL reads each complete line once, re-reading only incomplete lines on
// the next poll. reset discards decoder state if the file is replaced or shrinks.
// existing distinguishes lines already present when a file generation is opened.
func TailJSONL(ctx context.Context, path string, reset func(), onLine func(line []byte, offset int64, existing bool) error) error {
	tail := JSONLTail{Path: path}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := tail.Poll(ctx, reset, onLine)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// JSONLTail tracks the byte cursor for one physical rollout segment.
type JSONLTail struct {
	Path        string
	previous    os.FileInfo
	offset      int64
	initialSize int64
}

func (t *JSONLTail) Poll(ctx context.Context, reset func(), onLine func([]byte, int64, bool) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(t.Path)
	if err != nil {
		return err
	}
	if t.previous != nil && os.SameFile(t.previous, info) && t.offset == info.Size() {
		return nil
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return err
	}
	if t.previous == nil || !os.SameFile(t.previous, info) || t.offset > info.Size() {
		t.offset = 0
		t.initialSize = info.Size()
		reset()
	}
	t.previous = info
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return err
	}
	// Read a bounded snapshot. Any complete record arriving during this poll
	// is picked up next time, including a previously incomplete last line.
	reader := bufio.NewReader(io.LimitReader(f, info.Size()-t.offset))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		end := t.offset + int64(len(line))
		if err := onLine(line, t.offset, end <= t.initialSize); err != nil {
			return err
		}
		t.offset = end
	}
}
