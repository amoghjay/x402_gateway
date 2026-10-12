// Package buildinfo carries the version stamped into every binary at link time.
package buildinfo

// Version is overridden by the Makefile via -ldflags "-X ...buildinfo.Version=<git describe>".
var Version = "dev"
