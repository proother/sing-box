package ccm

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// acquireCredentialLock acquires a cross-process lock compatible with Claude Code's
// proper-lockfile protocol. The lock is a directory created via mkdir (atomic on
// POSIX filesystems).
//
// ref (@anthropic-ai/claude-code @2.1.81): cli.js _P1 (line 179530-179577)
// ref: proper-lockfile mkdir protocol (cli.js:43570)
// ref: proper-lockfile default options — stale=10s, update=stale/2=5s, realpath=true (cli.js:43661-43664)
//
// Claude Code locks d1() (= ~/.claude config dir). The lock directory is
// <realpath(configDir)>.lock (proper-lockfile default: <path>.lock).
// Manual retry: initial + 5 retries = 6 total, delay 1+rand(1s) per retry.
func acquireCredentialLock(configDir string) (func(), error) {
	// ref: cli.js _P1 line 179531 — mkdir -p configDir before locking
	os.MkdirAll(configDir, 0o700)
	// ref: proper-lockfile realpath:true (cli.js:43664) — resolve symlinks before appending .lock
	resolved, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		resolved = filepath.Clean(configDir)
	}
	lockPath := resolved + ".lock"
	// ref: cli.js _P1 line 179539-179543 — initial + 5 retries = 6 total attempts
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			// ref: cli.js _P1 line 179542 — 1000 + Math.random() * 1000
			delay := time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
			time.Sleep(delay)
		}
		err = os.Mkdir(lockPath, 0o755)
		if err == nil {
			return startLockHeartbeat(lockPath), nil
		}
		if !os.IsExist(err) {
			return nil, E.Cause(err, "create lock directory")
		}
		// ref: proper-lockfile stale check (cli.js:43603-43604)
		// stale threshold = 10s (cli.js:43662)
		info, statErr := os.Stat(lockPath)
		if statErr != nil {
			continue
		}
		if time.Since(info.ModTime()) > 10*time.Second {
			os.Remove(lockPath)
		}
	}
	return nil, E.New("credential lock timeout")
}

// startLockHeartbeat spawns a goroutine that touches the lock directory's mtime
// every 5 seconds to prevent stale detection by other processes.
//
// ref: proper-lockfile update interval = stale/2 = 5s (cli.js:43662-43663)
//
// Returns a release function that stops the heartbeat and removes the lock directory.
func startLockHeartbeat(lockPath string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				os.Chtimes(lockPath, now, now)
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		os.Remove(lockPath)
	}
}
