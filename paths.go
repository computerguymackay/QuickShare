package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// Paths holds every on-disk location quickshare uses.
// The base directory is ~/.quickshare, or $QUICKSHARE_HOME if set.
type Paths struct {
	Home       string
	Config     string
	DB         string
	Sock       string // Unix control socket
	CtrlAddr   string // Windows: file holding loopback address + key
	PID        string
	Log        string
	DaemonLock string // held by the running daemon for its whole life
	SpawnLock  string // held by a CLI while it probes/spawns a daemon
}

func resolvePaths() (Paths, error) {
	home := os.Getenv("QUICKSHARE_HOME")
	if home == "" {
		u, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("cannot find home directory: %w", err)
		}
		home = filepath.Join(u, ".quickshare")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, err
	}
	sock := filepath.Join(home, "control.sock")
	// Unix socket paths are limited to ~104-108 bytes. For a deep home
	// directory, use a short per-home name in the temp directory instead.
	if len(sock) > 100 {
		sum := sha256.Sum256([]byte(home))
		sock = filepath.Join(os.TempDir(), fmt.Sprintf("quickshare-%x.sock", sum[:8]))
	}
	return Paths{
		Home:       home,
		Config:     filepath.Join(home, "config.toml"),
		DB:         filepath.Join(home, "state.db"),
		Sock:       sock,
		CtrlAddr:   filepath.Join(home, "control.addr"),
		PID:        filepath.Join(home, "daemon.pid"),
		Log:        filepath.Join(home, "quickshare.log"),
		DaemonLock: filepath.Join(home, "daemon.lock"),
		SpawnLock:  filepath.Join(home, "spawn.lock"),
	}, nil
}

func (p Paths) ensure() error {
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.Home, 0o700)
}
