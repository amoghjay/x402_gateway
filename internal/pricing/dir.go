package pricing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

// DirWatcher keeps a Store in step with a directory, for a filter that runs without
// Kubernetes. The directory holds the same files a price-table ConfigMap holds as keys:
// prices.json, and for Radius Platform routes agents.json and policy-<project>.json. That
// is also the layout of a ConfigMap mounted as a volume, and what `x402-controller sync`
// writes, so one reader serves compose, a VM and an in-cluster mount.
//
// It reads the directory every Every (and at once on Reload) and applies it only when the
// files' contents changed. Polling, not file events: no dependency, and it sees through
// the symlink swap a mounted ConfigMap uses. A broken or half-written file is rejected
// like a broken ConfigMap: the last good table stays, and the next change is tried again.
type DirWatcher struct {
	Dir      string
	Keys     KeyStyle
	Networks []radius.Profile
	Store    *Store
	Log      *slog.Logger
	Every    time.Duration      // default DefaultDirPoll
	Reload   <-chan os.Signal   // SIGHUP: read now
	notify   func(applied bool) // tests: called after each read that saw a change
	last     [sha256.Size]byte
	seen     bool
}

// DefaultDirPoll is how often the directory is read.
const DefaultDirPoll = 5 * time.Second

// maxFileBytes caps each file at what a ConfigMap can hold, so a stray large file cannot
// make the filter read it every few seconds.
const maxFileBytes = 1 << 20

// Run reads the directory until ctx is done.
func (w *DirWatcher) Run(ctx context.Context) error {
	every := w.Every
	if every <= 0 {
		every = DefaultDirPoll
	}
	w.poll()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-w.Reload:
			w.Log.Info("reloading the price table directory", "dir", w.Dir)
		}
		w.poll()
	}
}

// poll reads the directory and applies it if anything changed since the last read.
func (w *DirWatcher) poll() {
	data, err := ReadDir(w.Dir)
	sum := digest(data, err)
	if w.seen && sum == w.last {
		return
	}
	w.seen, w.last = true, sum
	log := w.Log.With("dir", w.Dir)
	var applied bool
	if err != nil {
		log.Error("price table directory unreadable; serving the last good table", "err", err, "loaded", w.Store.Load() != nil)
	} else {
		applied = load(w.Store, data, w.Keys, w.Networks, log)
	}
	if w.notify != nil {
		w.notify(applied)
	}
}

// ReadDir reads a table directory's files by name: prices.json, agents.json and
// policy-*.json. Anything else is ignored, including the dot-entries of a mounted
// ConfigMap. A missing prices.json is not an error here; Build refuses it.
func ReadDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	data := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !tableFile(name) {
			continue
		}
		b, err := readCapped(filepath.Join(dir, name)) // follows a mounted ConfigMap's symlinks
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		data[name] = string(b)
	}
	return data, nil
}

func tableFile(name string) bool {
	return name == ConfigMapKey || name == AgentsKey ||
		(strings.HasPrefix(name, "policy-") && strings.HasSuffix(name, ".json"))
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path)) // a file in the operator-given table directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, errors.New("larger than 1 MiB")
	}
	return b, nil
}

// digest identifies what a read saw: the files and their contents, or the error.
func digest(data map[string]string, err error) [sha256.Size]byte {
	h := sha256.New()
	if err != nil {
		_, _ = io.WriteString(h, "error\x00"+err.Error())
	}
	for _, k := range slices.Sorted(maps.Keys(data)) {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s", k, len(data[k]), data[k])
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
