//go:build windows

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// The agent's own log file under the Windows service manager.
const (
	// logFileName is the log file's name inside the state directory.
	logFileName = "hanse-node.log"
	// logRotationSuffix names the one old file that a rotation keeps.
	logRotationSuffix = ".1"
	// logLimit is the size at which the log rotates. A rotation keeps one
	// old file, so the pair never exceeds twice this size.
	logLimit = 10 << 20
	// logBufferLimit is the most that is held in memory before the state
	// directory is known, so a startup problem still reaches the file.
	logBufferLimit = 64 << 10
)

// nodeLog is the agent's log file under the Windows service manager. The
// manager discards a service's standard error, so the service writes its
// own lines to <state dir>\hanse-node.log.
//
// The file is created inside the state directory, so it inherits that
// directory's access control list, which install restricts to
// Administrators, SYSTEM, and the service account. The agent never logs a
// credential or an elevation password, so the file holds no secret.
//
// A write never fails the agent: a log that cannot be written is dropped,
// because the agent must keep running even when the disk is full.
type nodeLog struct {
	mu   sync.Mutex
	dir  string
	file *os.File
	size int64
	// buf holds the lines written before the state directory is known.
	buf []byte
}

// openAgentLog returns the writer for the agent's own lines, and a function
// that releases it. A service has no usable standard error, so it writes to
// the log file; an agent started by an operator keeps standard error.
func openAgentLog(service bool, stderr io.Writer) (io.Writer, func()) {
	if !service {
		return stderr, func() {}
	}
	log := &nodeLog{}
	return log, func() { _ = log.Close() }
}

// SetStateDir moves the log into the state directory and writes the lines
// that arrived before it was known. The agent learns the directory from
// agent.yaml, so an early configuration problem is held in memory until
// then.
func (l *nodeLog) SetStateDir(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dir == "" || dir == l.dir {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.dir = dir
	l.size = 0
	l.openLocked()
}

// Write appends one line to the log file. It always reports success, so a
// failed write never stops the agent.
func (l *nodeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		if l.dir == "" {
			l.bufferLocked(p)
			return len(p), nil
		}
		l.openLocked()
		if l.file == nil {
			l.bufferLocked(p)
			return len(p), nil
		}
	}
	if l.size+int64(len(p)) > logLimit {
		l.rotateLocked()
	}
	if l.file != nil {
		if written, err := l.file.Write(p); err == nil {
			l.size += int64(written)
		}
	}
	return len(p), nil
}

// Close releases the log file. Lines that never reached it are dropped.
func (l *nodeLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = nil
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// bufferLocked keeps a line that arrived before the state directory was
// known, up to logBufferLimit.
func (l *nodeLog) bufferLocked(p []byte) {
	room := logBufferLimit - len(l.buf)
	if room <= 0 {
		return
	}
	if room < len(p) {
		p = p[:room]
	}
	l.buf = append(l.buf, p...)
}

// openLocked opens the log file in the current state directory and writes
// the held lines. The file rotates first when it already reached the limit,
// so a restart never grows it past the cap.
func (l *nodeLog) openLocked() {
	if l.dir == "" || l.file != nil {
		return
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return
	}
	path := l.path()
	if info, err := os.Stat(path); err == nil && info.Size() >= logLimit {
		rotateFile(path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.file = file
	l.size = 0
	if info, err := file.Stat(); err == nil {
		l.size = info.Size()
	}
	if len(l.buf) > 0 {
		if written, err := file.Write(l.buf); err == nil {
			l.size += int64(written)
		}
		l.buf = nil
	}
}

// rotateLocked moves the log to its one old name and starts a new file.
func (l *nodeLog) rotateLocked() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	path := l.path()
	rotateFile(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.file = file
	l.size = 0
}

// rotateFile renames path to path.1, replacing the old file. When the
// rename fails, the current file keeps growing and the next write tries
// again; nothing is truncated, because a log is worth more than its cap.
func rotateFile(path string) {
	_ = os.Remove(path + logRotationSuffix)
	if err := os.Rename(path, path+logRotationSuffix); err != nil &&
		!errors.Is(err, fs.ErrNotExist) {
		return
	}
}

// path is the log file's path in the current state directory.
func (l *nodeLog) path() string {
	return filepath.Join(l.dir, logFileName)
}
