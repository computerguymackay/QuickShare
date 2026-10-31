package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ensureDaemon returns the status of a running daemon, starting one if
// needed. spawned reports whether this call started it.
func ensureDaemon(p Paths, opt DaemonOptions) (st DaemonStatus, spawned bool, err error) {
	if st, err := daemonStatus(p); err == nil {
		return st, false, nil
	}
	if err := p.ensure(); err != nil {
		return st, false, err
	}

	// Serialise probe+spawn so parallel invocations start one daemon.
	release, err := lockFile(p.SpawnLock, true)
	if err != nil {
		return st, false, fmt.Errorf("spawn lock: %w", err)
	}
	defer release()

	if st, err := daemonStatus(p); err == nil {
		return st, false, nil
	}
	clearStale(p)

	exe, err := os.Executable()
	if err != nil {
		return st, false, err
	}
	rotateLog(p.Log)
	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return st, false, err
	}
	defer logf.Close()

	args := []string{"__daemon", "--bind", opt.Bind}
	if opt.TLSCert != "" {
		args = append(args, "--tls-cert", opt.TLSCert, "--tls-key", opt.TLSKey)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = p.Home
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachAttr()
	logStart, _ := logf.Seek(0, 2)
	if err := cmd.Start(); err != nil {
		return st, false, fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(10 * time.Second)
	for {
		if st, err := daemonStatus(p); err == nil {
			return st, true, nil
		}
		select {
		case <-exited:
			// It may have lost a race to another daemon that is now up.
			if st, err := daemonStatus(p); err == nil {
				return st, false, nil
			}
			return st, false, fmt.Errorf("daemon failed to start:\n%s", logSince(p.Log, logStart))
		case <-deadline:
			return st, false, fmt.Errorf("daemon did not become ready within 10s; see %s", p.Log)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// clearStale removes a PID file and socket left by a daemon that died.
// If the daemon lock is free, no daemon is alive.
func clearStale(p Paths) {
	release, err := lockFile(p.DaemonLock, false)
	if err != nil {
		return // a daemon holds it (maybe still starting up)
	}
	defer release()
	os.Remove(p.PID)
	cleanupControl(p)
}

// daemonAlive reports whether a daemon process holds the daemon lock.
func daemonAlive(p Paths) bool {
	release, err := lockFile(p.DaemonLock, false)
	if err != nil {
		return errors.Is(err, errLocked)
	}
	release()
	return false
}

func rotateLog(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".1")
	}
}

func logSince(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil || offset > int64(len(b)) {
		return "  (no log output)"
	}
	s := strings.TrimSpace(string(b[offset:]))
	if s == "" {
		return "  (no log output)"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
