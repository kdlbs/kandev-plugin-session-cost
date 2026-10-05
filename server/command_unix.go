//go:build !windows

package main

func platformSourceTimezone() string { return "UTC" }
