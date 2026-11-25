package pricing

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/metrics/metricstest"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

type dirFixture struct {
	t      *testing.T
	dir    string
	w      *DirWatcher
	events chan bool
}

func newDirFixture(t *testing.T) *dirFixture {
	t.Helper()
	f := &dirFixture{t: t, dir: t.TempDir(), events: make(chan bool, 16)}
	f.w = &DirWatcher{
		Dir: f.dir, Keys: KeysName, Networks: []radius.Profile{radius.Testnet}, Store: NewStore(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), notify: func(applied bool) { f.events <- applied },
	}
	return f
}

// table is the example table with its one route renamed and priced.
func (f *dirFixture) table(name, price string) string {
	s := strings.Replace(string(example(f.t)), "httproute/demo/httpbin", name, 1)
	return strings.Replace(s, `"price": "1000"`, `"price": "`+price+`"`, 1)
}

func (f *dirFixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dirFixture) price(route string) string {
	e, ok := f.w.Store.Load().Lookup(route)
	if !ok {
		return ""
	}
	return e.Price.String()
}

func TestDirWatcherLoadsReloadsAndKeepsTheLastGood(t *testing.T) {
	f := newDirFixture(t)
	f.write(ConfigMapKey, f.table("weather", "1000"))
	f.w.poll()
	if !<-f.events || f.price("weather") != "1000" {
		t.Fatalf("first load: price %q", f.price("weather"))
	}

	// Nothing changed: nothing is applied again.
	f.w.poll()
	select {
	case <-f.events:
		t.Error("an unchanged directory was applied again")
	default:
	}

	f.write(ConfigMapKey, f.table("weather", "2000"))
	f.w.poll()
	if !<-f.events || f.price("weather") != "2000" {
		t.Errorf("edit: price %q, want 2000", f.price("weather"))
	}

	// A half-written file: rejected, counted, and the table in force stays.
	rejected := metricstest.Value(t, "x402_price_table_rejected_total", map[string]string{})
	f.write(ConfigMapKey, `{"schemaVersion": 1, "routes": {`)
	f.w.poll()
	if <-f.events || f.price("weather") != "2000" {
		t.Errorf("broken file: price %q, want the last good 2000", f.price("weather"))
	}
	if got := metricstest.Value(t, "x402_price_table_rejected_total", map[string]string{}); got != rejected+1 {
		t.Errorf("rejections = %v, want %v", got, rejected+1)
	}

	// The file removed: still the last good table, never free.
	if err := os.Remove(filepath.Join(f.dir, ConfigMapKey)); err != nil {
		t.Fatal(err)
	}
	f.w.poll()
	if <-f.events || f.price("weather") != "2000" {
		t.Errorf("deleted file: price %q, want the last good 2000", f.price("weather"))
	}
}

// Before any table, a missing prices.json leaves the store empty, so the filter is not
// ready; files that are not table files are ignored.
func TestDirWatcherWithoutATable(t *testing.T) {
	f := newDirFixture(t)
	f.write("README.md", "not a table")
	f.w.poll()
	if <-f.events || f.w.Store.Load() != nil {
		t.Error("a directory without prices.json produced a table")
	}
	data, err := ReadDir(f.dir)
	if err != nil || len(data) != 0 {
		t.Errorf("ReadDir = %v, %v; want no table files", data, err)
	}
}

// SIGHUP reads the directory at once, without waiting for the next poll.
func TestDirWatcherReloadsOnSignal(t *testing.T) {
	f := newDirFixture(t)
	hup := make(chan os.Signal, 1)
	f.w.Reload, f.w.Every = hup, time.Hour
	f.write(ConfigMapKey, f.table("weather", "1000"))
	go func() { _ = f.w.Run(t.Context()) }()
	<-f.events // the first read

	f.write(ConfigMapKey, f.table("weather", "3000"))
	hup <- syscall.SIGHUP
	select {
	case applied := <-f.events:
		if !applied || f.price("weather") != "3000" {
			t.Errorf("after SIGHUP: price %q, want 3000", f.price("weather"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGHUP did not trigger a read")
	}
}
