//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// On Windows the control channel is loopback TCP. Any local user could
// connect to it, so requests must carry a random key that is stored in
// a file only the owner can read.
func listenControl(p Paths) (net.Listener, string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	key, err := newToken()
	if err != nil {
		l.Close()
		return nil, "", err
	}
	if err := os.WriteFile(p.CtrlAddr, []byte(l.Addr().String()+" "+key), 0o600); err != nil {
		l.Close()
		return nil, "", err
	}
	return l, key, nil
}

func dialControl(p Paths) (net.Conn, string, error) {
	data, err := os.ReadFile(p.CtrlAddr)
	if err != nil {
		return nil, "", err
	}
	addr, key, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	if !ok {
		return nil, "", fmt.Errorf("malformed %s", p.CtrlAddr)
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	return c, key, err
}

func cleanupControl(p Paths) { os.Remove(p.CtrlAddr) }

var errLocked = errors.New("lock is held by another process")

func lockFile(path string, block bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, flags, 0, 1, 0, ol); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { windows.UnlockFileEx(h, 0, 1, 0, ol); f.Close() }, nil
}

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

var stopSignals = []os.Signal{os.Interrupt}
