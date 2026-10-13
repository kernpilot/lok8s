package cache

// dryrun.go — the read-only store a dry run (`lo bootstrap --dry-run`)
// renders against. lo sets LOK8S_BOOTSTRAP_DRYRUN=1 for the whole run, so
// the plugin reads the existing store, generates a missing value in memory
// and never writes a file or creates a directory. Each value generated in
// memory is reported to the file LOK8S_BOOTSTRAP_DRYRUN_REPORT names: lo
// marks that entry "non-repeatable", because the real run generates new
// bytes for it.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

const (
	// DryRunEnv is set to "1" by lo for a dry run.
	DryRunEnv = "LOK8S_BOOTSTRAP_DRYRUN"
	// DryRunReportEnv names the file a dry run reports generated material
	// to. lo creates the file before the render; the plugin only appends.
	DryRunReportEnv = "LOK8S_BOOTSTRAP_DRYRUN_REPORT"
)

// Store is the cache surface a generator uses. *Cache and the dry-run
// overlay implement it (structurally equal to plugin.CacheStore).
type Store interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Has(key string) bool
	GetOrCreate(key string, gen func() ([]byte, error)) ([]byte, error)
	ReadByName(filename string) ([]byte, error)
	Dir() string
}

// DryRun is the in-memory state of one dry-run plugin process: the values
// generated so far (keyed by their full store path, so two stores of the
// same process share them) and the report file.
type DryRun struct {
	mu     sync.Mutex
	mem    map[string][]byte
	report string
}

// DryRunFromEnv returns the dry-run state when env carries
// LOK8S_BOOTSTRAP_DRYRUN=1, else nil (a normal run).
func DryRunFromEnv(env func(string) (string, bool)) *DryRun {
	if v, _ := env(DryRunEnv); v != "1" {
		return nil
	}
	report, _ := env(DryRunReportEnv)
	return &DryRun{mem: map[string][]byte{}, report: report}
}

// Minted reports generated material (a store entry, or a CA) to the report
// file. It never writes the material itself, only its name. A report file
// that cannot be opened is an error: lo must not show an entry as
// repeatable when it is not. Without a report file (a dry run that lo did
// not start) it does nothing.
func (d *DryRun) Minted(what string) error {
	if d == nil || d.report == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// No O_CREATE: lo created the file. The plugin never creates one.
	f, err := os.OpenFile(d.report, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("dry-run report: %w", err)
	}
	if _, err := fmt.Fprintln(f, what); err != nil {
		_ = f.Close()
		return fmt.Errorf("dry-run report: %w", err)
	}
	return f.Close()
}

// Open returns the store for one (namespace, name) under dir. Outside a dry
// run (dr nil) it is New: the on-disk cache, created when missing. In a dry
// run it is the read-only overlay: no directory is created, no file is
// written.
func Open(dir, namespace, name string, dr *DryRun) (Store, error) {
	if dr == nil {
		return New(dir, namespace, name)
	}
	if dir == "" {
		return nil, errs.New("cache: PATH_SECRETS is not set")
	}
	if name == "" {
		return nil, errs.New("cache: secret name is empty")
	}
	return &overlay{disk: &Cache{dir: dir, namespace: namespace, name: name}, dr: dr}, nil
}

// overlay reads the on-disk store and keeps every write in memory.
type overlay struct {
	disk *Cache
	dr   *DryRun
}

func (o *overlay) Dir() string { return o.disk.dir }

func (o *overlay) memGet(path string) ([]byte, bool) {
	o.dr.mu.Lock()
	defer o.dr.mu.Unlock()
	v, ok := o.dr.mem[path]
	return v, ok
}

func (o *overlay) memPut(path string, v []byte) {
	o.dr.mu.Lock()
	defer o.dr.mu.Unlock()
	o.dr.mem[path] = append([]byte(nil), v...)
}

func (o *overlay) Get(key string) ([]byte, error) {
	if v, ok := o.memGet(o.disk.filename(key)); ok {
		return v, nil
	}
	return o.disk.Get(key)
}

func (o *overlay) Has(key string) bool {
	if _, ok := o.memGet(o.disk.filename(key)); ok {
		return true
	}
	return o.disk.Has(key)
}

// Put keeps the value in memory. A Put stores a value the generator read
// (an env var, a default, a command's output), not one it generated, so it
// is not reported.
func (o *overlay) Put(key string, value []byte) error {
	if cur, err := o.disk.Get(key); err == nil && bytes.Equal(cur, value) {
		return nil
	}
	o.memPut(o.disk.filename(key), value)
	return nil
}

// GetOrCreate returns the stored value; on a miss it generates the value in
// memory and reports it.
func (o *overlay) GetOrCreate(key string, gen func() ([]byte, error)) ([]byte, error) {
	if v, err := o.Get(key); err == nil {
		return v, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	v, err := gen()
	if err != nil {
		return nil, err
	}
	if err := o.dr.Minted("secret " + FormatName(o.disk.name, o.disk.namespace, key)); err != nil {
		return nil, err
	}
	o.memPut(o.disk.filename(key), v)
	return v, nil
}

// ReadByName reads another Secret's entry (secretRef). A miss is reported
// too: in a real run, an earlier generator of the same render can write the
// entry first, and a dry run writes nothing, so the value is not readable
// here.
func (o *overlay) ReadByName(filename string) ([]byte, error) {
	if _, err := o.disk.checkName(filename); err != nil {
		return nil, err
	}
	if v, ok := o.memGet(filepath.Join(o.disk.dir, filename)); ok {
		return v, nil
	}
	v, err := o.disk.ReadByName(filename)
	if errors.Is(err, os.ErrNotExist) {
		if rerr := o.dr.Minted("secretRef " + filename); rerr != nil {
			return nil, rerr
		}
		return nil, fmt.Errorf("%w (a dry run writes no store entry, so a value that another Secret of this render generates first is not readable; run `lo build` or the real bootstrap once)", err)
	}
	return v, err
}
