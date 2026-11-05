//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/kdlbs/kandev/pluginsdk"
)

func servePlugin(p *plugin) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		p.Close()
		os.Exit(0)
	}()
	pluginsdk.Serve(p)
	p.Close()
}
