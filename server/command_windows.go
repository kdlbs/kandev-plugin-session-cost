//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

var windowsSourceTimezone = sync.OnceValue(func() string {
	// Node/ICU maps the Windows system zone to its IANA identity, including
	// daylight saving rules. It uses the same system zone as tokscale.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "node", "-p", "Intl.DateTimeFormat().resolvedOptions().timeZone").Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(output))
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
})

func platformSourceTimezone() string { return windowsSourceTimezone() }
