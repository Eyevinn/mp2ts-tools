//go:build unix

package internal

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestReadInputFromPipe checks that an input that cannot be mapped, a named
// pipe, is read instead.
func TestReadInputFromPipe(t *testing.T) {
	content := bytes.Repeat([]byte{syncByte, 0x1f, 0xff, 0x10}, 100_000)
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("no named pipes:", err)
	}
	written := make(chan error, 1)
	go func() { written <- os.WriteFile(path, content, 0o600) }()
	data, release, err := readInput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !bytes.Equal(data, content) {
		t.Errorf("read %d bytes of %d", len(data), len(content))
	}
	if err := <-written; err != nil {
		t.Error(err)
	}

	// A file is mapped, and an empty one refused.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	data, release, err = readInput(file)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("read %d bytes of %d: %v", len(data), len(content), err)
	}
	release()
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readInput(file); err == nil {
		t.Error("an empty file is accepted")
	}
}
