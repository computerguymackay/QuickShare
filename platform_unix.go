//go:build !windows

package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// listenControl opens the Unix control socket. The caller must hold the
// daemon lock, so any existing socket file is stale and safe to remove.
func listenControl(p Paths) (net.Listener, string, error) {
	os.Remove(p.Sock)
	old := syscall.Umask(0o077)
	l, err := net.Listen("unix", p.Sock)
	syscall.Umask(old)
	if err != nil {
		return nil, "", err
	}
	os.Chmod(p.Sock, 0o600)
	return l, "", nil
}

func dialControl(p Paths) (net.Conn, string, error) {
	c, err := net.DialTimeout("unix", p.Sock, 2*time.Second)
	return c, "", err
}

func cleanupControl(p Paths) { os.Remove(p.Sock) }

var errLocked = errors.New("lock is held by another process")

// lockFile takes an exclusive advisory lock. It is released when the
// returned function runs or the process dies.
func lockFile(path string, block bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

var stopSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
