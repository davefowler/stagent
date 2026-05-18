package runner

import (
	"os"
	"os/signal"
	"syscall"
)

// signalNotify is a thin wrapper so tests can stub it. The runner
// listens for SIGINT, SIGTERM, and SIGHUP — same set as a typical
// long-running daemon.
func signalNotify(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func signalStop(ch chan<- os.Signal) {
	signal.Stop(ch)
}
