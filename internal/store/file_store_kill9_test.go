package store

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Crash-recovery test (MYC-83). A child process writes records to a FileStore
// in a loop and reports each Put it has seen return. The parent sends SIGKILL
// while the child is still writing, then reopens the database.
//
// What this proves: a commit that returned before the kill is present after
// restart; a kill in the middle of a commit leaves a loadable database that
// holds a contiguous prefix of the writes; the kernel releases the
// single-writer lock on process death, so the next open succeeds.
//
// What this does NOT prove: power loss or kernel crash. The commit path calls
// fsync on the temporary file and on the directory, but that durability cannot
// be observed from user space in a test. It also does not prove the outcome of
// a write that was in flight when the kill landed; that write may or may not
// be present, and the test only requires that the database is consistent.

const (
	kill9HelperEnv   = "TELEADMIN_KILL9_HELPER"
	kill9DBEnv       = "TELEADMIN_KILL9_DB"
	kill9Table       = "kill9"
	kill9MinCommits  = 20
	kill9WaitTimeout = 30 * time.Second
)

// TestFileStoreKill9Helper is the child process body. It runs only when
// re-executed by TestFileStoreSurvivesSIGKILL and skips in a normal test run.
func TestFileStoreKill9Helper(t *testing.T) {
	if os.Getenv(kill9HelperEnv) != "1" {
		t.Skip("helper process for TestFileStoreSurvivesSIGKILL")
	}
	fs, err := OpenFileStore(os.Getenv(kill9DBEnv))
	if err != nil {
		fmt.Fprintf(os.Stdout, "HELPER-ERROR %v\n", err)
		os.Exit(3)
	}
	for i := 0; ; i++ {
		if err := fs.Put(kill9Table, strconv.Itoa(i), map[string]int{"i": i}); err != nil {
			fmt.Fprintf(os.Stdout, "HELPER-ERROR %v\n", err)
			os.Exit(4)
		}
		fmt.Fprintf(os.Stdout, "COMMITTED %d\n", i)
	}
}

func TestFileStoreSurvivesSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("kill -9 test starts a child process")
	}
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileStoreKill9Helper$", "-test.count=1")
	cmd.Env = append(os.Environ(), kill9HelperEnv+"=1", kill9DBEnv+"="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Read the child's commit reports until enough commits are durable, then
	// kill it while it is still writing.
	reached := make(chan int, 1)
	helperErr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		last := -1
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "HELPER-ERROR"):
				helperErr <- line
				return
			case strings.HasPrefix(line, "COMMITTED "):
				n, err := strconv.Atoi(strings.TrimPrefix(line, "COMMITTED "))
				if err != nil {
					continue
				}
				last = n
				if n >= kill9MinCommits {
					reached <- last
					return
				}
			}
		}
		helperErr <- fmt.Sprintf("helper exited before %d commits (last %d)", kill9MinCommits, last)
	}()

	var committed int
	select {
	case committed = <-reached:
	case msg := <-helperErr:
		t.Fatalf("helper: %s", msg)
	case <-time.After(kill9WaitTimeout):
		t.Fatalf("helper did not reach %d commits within %s", kill9MinCommits, kill9WaitTimeout)
	}

	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	_ = cmd.Wait()
	reaped = true

	// The lock must be released by the kernel on process death, so a fresh
	// open succeeds. Its contents must be a contiguous prefix that includes
	// every commit the child reported.
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen after SIGKILL: %v", err)
	}
	defer fs.Close()
	keys := fs.Keys(kill9Table)
	if len(keys) < committed+1 {
		t.Fatalf("after SIGKILL, %d records present; the child reported commits up to %d", len(keys), committed)
	}
	idx := make([]int, 0, len(keys))
	for _, k := range keys {
		n, err := strconv.Atoi(k)
		if err != nil {
			t.Fatalf("unexpected record key %q", k)
		}
		idx = append(idx, n)
	}
	sort.Ints(idx)
	for i, n := range idx {
		if n != i {
			t.Fatalf("records are not a contiguous prefix: position %d holds %d", i, n)
		}
		var rec map[string]int
		if ok, err := fs.Get(kill9Table, strconv.Itoa(n), &rec); err != nil || !ok || rec["i"] != n {
			t.Fatalf("record %d unreadable after SIGKILL: ok=%v err=%v rec=%v", n, ok, err, rec)
		}
	}
	t.Logf("SIGKILL recovery: child reported %d commits; %d records loaded", committed, len(keys))
}
