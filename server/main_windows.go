//go:build windows

package main

import (
	"os"
	"os/signal"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

func servePlugin(p *plugin) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	go func() {
		<-signals
		p.Close()
		os.Exit(0)
	}()
	pluginsdk.Serve(p)
	p.Close()
}
