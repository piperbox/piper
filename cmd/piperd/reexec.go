package main

import (
	"errors"
	"log"
	"os"
	"runtime"
	"syscall"
)

// bootExe pins the identity (device+inode) of the binary this process booted
// from. execSelf refuses to re-exec when the on-disk executable no longer
// matches: on a root install whose program path a non-root user can write
// (sudo-brew macOS), an unchecked re-exec would run a replaced binary as root
// from a loopback-triggerable endpoint. A mismatch falls back to exit(1) so
// the supervisor relaunches the (new) binary under its own hardening.
var bootExe struct {
	dev, ino uint64
	ok       bool
}

func captureBootExe() {
	path, err := os.Executable()
	if err != nil {
		return
	}
	if dev, ino, err := statDevIno(path); err == nil {
		bootExe.dev, bootExe.ino, bootExe.ok = dev, ino, true
	}
}

func statDevIno(path string) (uint64, uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("no stat_t")
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// Seams so tests observe the exec/exit decision without leaving the process.
var execFn = syscall.Exec
var exitFn = os.Exit

// execSelf replaces this process with a fresh boot of the same image — same
// PID, so systemd/launchd observe nothing, and the new boot re-runs
// config.Load and all the relay wiring. Callers must have fully drained
// first. On any doubt it exits 1 instead: Restart=on-failure and brew
// keep_alive relaunch piperd supervised, re-reading EnvironmentFile and
// re-applying sandboxing. Never exit 0 here — on systemd that leaves the box
// down for good.
func execSelf() {
	path, err := os.Executable()
	if err != nil {
		log.Printf("re-exec: cannot resolve executable: %v; exiting for a supervised restart", err)
		exitFn(1)
		return
	}
	if dev, ino, err := statDevIno(path); err != nil || !bootExe.ok || dev != bootExe.dev || ino != bootExe.ino {
		log.Printf("re-exec: binary changed since boot; exiting for a supervised restart")
		exitFn(1)
		return
	}
	if runtime.GOOS == "linux" {
		path = "/proc/self/exe" // immune to on-disk replacement races
	}
	log.Printf("applying enrollment: re-exec %s", path)
	if err := execFn(path, os.Args, os.Environ()); err != nil {
		log.Printf("re-exec failed: %v; exiting for a supervised restart", err)
		exitFn(1)
	}
}
