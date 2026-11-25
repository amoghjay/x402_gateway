package main

import (
	"bytes"
	"testing"
)

func TestRunSyncRefusesBadArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"--nope"}, 2},
		{[]string{"--interval", "100ms"}, 2},
		{[]string{"extra"}, 2},
		{[]string{"--out", t.TempDir()}, 1}, // no project, key or route: refused before any fetch
	} {
		var stderr bytes.Buffer
		if code := runSync(c.args, &stderr); code != c.code {
			t.Errorf("sync %v = %d, want %d (%s)", c.args, code, c.code, stderr.String())
		}
	}
}
