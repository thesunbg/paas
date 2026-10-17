package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startFollow runs Follow on a fresh log file and returns its path and the
// line channel. It returns only once Follow holds the file
// open (it seeks to the end on open, so earlier writes would be skipped).
func startFollow(t *testing.T) (path string, lines chan string) {
	t.Helper()
	r := &Runtime{dataDir: t.TempDir(), containers: map[string]*managed{}}
	path = r.LogPath("c1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lines = make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Follow(ctx, "c1", func(l string) { lines <- l })
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for !fdOpen(path) {
		if time.Now().After(deadline) {
			t.Fatal("Follow did not open the log")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the Seek(end) after open finish
	return path, lines
}

func fdOpen(path string) bool {
	ents, _ := os.ReadDir("/proc/self/fd")
	for _, e := range ents {
		if p, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && p == path {
			return true
		}
	}
	return false
}

func appendTo(t *testing.T, path string, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func nextLine(t *testing.T, lines chan string) string {
	t.Helper()
	select {
	case l := <-lines:
		return l
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a line")
		return ""
	}
}

func TestFollowLongLine(t *testing.T) {
	path, lines := startFollow(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	block := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 200; i++ {
		if _, err := f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString("\nok\n"); err != nil { // leading newline ends the long line
		t.Fatal(err)
	}

	first := nextLine(t, lines)
	if len(first) > maxLineBytes+len(truncatedMark) || !strings.HasSuffix(first, truncatedMark) {
		t.Fatalf("first line: len=%d suffix=%q", len(first), first[max(0, len(first)-20):])
	}
	if second := nextLine(t, lines); second != "ok" {
		t.Fatalf("second line = %.40q", second)
	}
	select {
	case l := <-lines:
		t.Fatalf("unexpected extra line %.40q", l)
	case <-time.After(700 * time.Millisecond): // > 2 polls
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	if grow := int64(after.HeapInuse) - int64(before.HeapInuse); grow > 8<<20 {
		t.Fatalf("HeapInuse grew %d bytes", grow)
	}
}

func TestFollowPartialAcrossPolls(t *testing.T) {
	path, lines := startFollow(t)
	appendTo(t, path, "ab")
	time.Sleep(700 * time.Millisecond)
	appendTo(t, path, "c\n")
	if l := nextLine(t, lines); l != "abc" {
		t.Fatalf("got %q", l)
	}
}

func TestFollowAfterTruncate(t *testing.T) {
	path, lines := startFollow(t)
	appendTo(t, path, "a\n")
	if l := nextLine(t, lines); l != "a" {
		t.Fatalf("got %q", l)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // let Follow notice the shrink
	appendTo(t, path, "b\n")
	if l := nextLine(t, lines); l != "b" {
		t.Fatalf("got %q", l)
	}
}

func TestCapLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	shim, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()

	// Under the cap: untouched.
	small := bytes.Repeat([]byte("s"), maxLogBytes-1)
	if _, err := shim.Write(small); err != nil {
		t.Fatal(err)
	}
	capLog(path)
	if st, _ := os.Stat(path); st.Size() != int64(len(small)) {
		t.Fatalf("small log size = %d", st.Size())
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("small log was backed up")
	}

	// Over the cap: .1 holds exactly the last maxLogBytes.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	const total = 15 << 20
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i*31 + i>>8)
	}
	if _, err := shim.Write(data); err != nil {
		t.Fatal(err)
	}
	capLog(path)
	got, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[total-maxLogBytes:]) {
		t.Fatalf(".1 is %d bytes, want the last %d", len(got), maxLogBytes)
	}
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Fatalf("original size = %d, want 0", st.Size())
	}

	// The shim's old fd keeps appending at the new end: no sparse hole.
	if _, err := shim.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() != 2 {
		t.Fatalf("original size after write = %d, want 2", st.Size())
	}
}
