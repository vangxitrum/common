package logger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// FileConfig configures the buffered rotating file sink.
type FileConfig struct {
	// Path is the log file path (required).
	Path string
	// Format controls line serialization. Defaults to FormatText (no ANSI colors).
	Format Format
	// BufferSize is the write buffer size in bytes. Defaults to 64 KB.
	// This bounds the maximum in-memory data not yet written to disk.
	BufferSize int
	// FlushInterval is how often the buffer is flushed to disk. Defaults to 5s.
	FlushInterval time.Duration
	// MaxSize is the file size threshold in bytes that triggers rotation.
	// Defaults to 100 MB. 0 disables size-based rotation.
	MaxSize int64
	// MaxBackups is the number of rotated backup files to keep (e.g. app.log.1,
	// app.log.2, …). 0 means keep all backups (timestamp-named).
	MaxBackups int
}

type fileSink struct {
	cfg  FileConfig
	mu   sync.Mutex
	file *os.File
	buf  *bufio.Writer
	size int64
	done chan struct{}
	wg   sync.WaitGroup
}

func newFileSink(cfg FileConfig) (*fileSink, error) {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 64 * 1024
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100 * 1024 * 1024
	}

	f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	s := &fileSink{
		cfg:  cfg,
		file: f,
		buf:  bufio.NewWriterSize(f, cfg.BufferSize),
		size: info.Size(),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.flushLoop()
	return s, nil
}

// Handle formats and appends a log record to the buffered file.
// The bufio.Writer enforces the memory budget: it flushes automatically when
// the buffer is full, keeping heap usage bounded to BufferSize.
func (s *fileSink) Handle(level slog.Level, t time.Time, msg string, attrs map[string]any) error {
	line := s.formatLine(level, t, msg, attrs)

	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := fmt.Fprintln(s.buf, line)
	if err != nil {
		return err
	}
	s.size += int64(n)

	if s.cfg.MaxSize > 0 && s.size >= s.cfg.MaxSize {
		return s.rotate()
	}
	return nil
}

func (s *fileSink) formatLine(level slog.Level, t time.Time, msg string, attrs map[string]any) string {
	if s.cfg.Format == FormatJSON {
		m := make(map[string]any, len(attrs)+3)
		for k, v := range attrs {
			if k == "logged_at" {
				continue
			}
			m[k] = v
		}
		m["time"] = t.Format(time.RFC3339Nano)
		m["level"] = levelStr(level)
		m["msg"] = msg
		b, _ := json.Marshal(m)
		return string(b)
	}

	var sb strings.Builder
	sb.WriteString(t.Format(timeFormat))
	sb.WriteByte(' ')
	sb.WriteString(levelStr(level))
	sb.WriteByte(' ')
	sb.WriteString(msg)
	for k, v := range attrs {
		if k == "logged_at" {
			continue
		}
		sb.WriteByte(' ')
		sb.WriteString(k)
		sb.WriteByte('=')
		fmt.Fprintf(&sb, "%v", v)
	}
	return sb.String()
}

func (s *fileSink) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			_ = s.buf.Flush()
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}

// rotate flushes the buffer, renames the current file, and opens a fresh one.
// Caller must hold s.mu.
func (s *fileSink) rotate() error {
	if err := s.buf.Flush(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}

	if s.cfg.MaxBackups > 0 {
		// Delete the oldest backup to make room, then shift .1….(N-1) → .2….N
		_ = os.Remove(fmt.Sprintf("%s.%d", s.cfg.Path, s.cfg.MaxBackups))
		for i := s.cfg.MaxBackups - 1; i >= 1; i-- {
			_ = os.Rename(
				fmt.Sprintf("%s.%d", s.cfg.Path, i),
				fmt.Sprintf("%s.%d", s.cfg.Path, i+1),
			)
		}
		_ = os.Rename(s.cfg.Path, s.cfg.Path+".1")
	} else {
		// Unlimited backups: use a timestamp suffix to avoid collisions.
		ts := time.Now().UTC().Format("20060102-150405.000")
		_ = os.Rename(s.cfg.Path, s.cfg.Path+"."+ts)
	}

	f, err := os.OpenFile(s.cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	s.buf = bufio.NewWriterSize(f, s.cfg.BufferSize)
	s.size = 0
	return nil
}

// Close stops the flush goroutine, performs a final flush, and closes the file.
// This is called by Logger.Shutdown after the async queue is fully drained, so
// all records have already been written to the buffer before this runs.
func (s *fileSink) Close() error {
	close(s.done)
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.buf.Flush(); err != nil {
		_ = s.file.Close()
		return err
	}
	return s.file.Close()
}
