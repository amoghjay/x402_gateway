package buildinfo

import "testing"

func TestVersionDefaultsToDev(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must never be empty: logs and metrics label on it")
	}
}
