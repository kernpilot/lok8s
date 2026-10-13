package cache

// dryrun.go — the read-only store a dry run (`lo bootstrap --dry-run`)
// renders against. lo sets LOK8S_BOOTSTRAP_DRYRUN=1 for the whole run, so
// the plugin reads the existing store and never writes a file into it or
// creates it. A value the plugin generates (or an env value it would update)
// goes to the RUN store instead: LOK8S_BOOTSTRAP_DRYRUN_STORE, a 0700
// directory lo owns for the length of the run and removes afterwards, so a
// later Secret of the same run (another plugin process) reads it back. Each
// such value is reported (by name only) to LOK8S_BOOTSTRAP_DRYRUN_REPORT:
// lo marks that entry "non-repeatable", because the real run generates new
// bytes for it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	// DryRunStoreEnv names the run store: the 0700 directory lo creates for
	// one dry run, where a value generated in this run is kept for the later
	// plugin processes of the same run. Unset, the values stay in memory.
	DryRunStoreEnv = "LOK8S_BOOTSTRAP_DRYRUN_STORE"
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

// DryRun is the state of one dry-run plugin process: the run store (or,
// without one, an in-memory map) and the report file. Values are keyed by
// the real store directory and their store file name, so two stores share
// them and two stores of different domains never mix.
type DryRun struct {
	mu     sync.Mutex
	mem    map[string][]byte
	store  string
	report string
}

// DryRunFromEnv returns the dry-run state when env carries
// LOK8S_BOOTSTRAP_DRYRUN=1, else nil (a normal run).
func DryRunFromEnv(env func(string) (string, bool)) *DryRun {
	if v, _ := env(DryRunEnv); v != "1" {
		return nil
	}
	report, _ := env(DryRunReportEnv)
	store, _ := env(DryRunStoreEnv)
	return &DryRun{mem: map[string][]byte{}, store: store, report: report}
}

// runKey is where the run store keeps a value of the real store dir.
func (d *DryRun) runKey(dir, name string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return filepath.Join(hex.EncodeToString(sum[:8]), name)
}

// Load reads a value this run generated (key: see runKey, or any name).
func (d *DryRun) Load(key string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store == "" {
		v, ok := d.mem[key]
		return v, ok
	}
	v, err := os.ReadFile(filepath.Join(d.store, key))
	return v, err == nil
}

// Save keeps a value for the rest of the run: in the run store (0700
// directories, 0600 files, written atomically), else in memory.
func (d *DryRun) Save(key string, v []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store == "" {
		d.mem[key] = append([]byte(nil), v...)
		return nil
	}
	path := filepath.Join(d.store, key)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("dry-run store: %w", err)
	}
	return writeFileAtomic(path, v, filePerm)
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

func (o *overlay) runGet(name string) ([]byte, bool) {
	return o.dr.Load(o.dr.runKey(o.disk.dir, name))
}

func (o *overlay) runPut(name string, v []byte) error {
	return o.dr.Save(o.dr.runKey(o.disk.dir, name), v)
}

func (o *overlay) Get(key string) ([]byte, error) {
	if v, ok := o.runGet(FormatName(o.disk.name, o.disk.namespace, key)); ok {
		return v, nil
	}
	return o.disk.Get(key)
}

func (o *overlay) Has(key string) bool {
	if _, ok := o.runGet(FormatName(o.disk.name, o.disk.namespace, key)); ok {
		return true
	}
	return o.disk.Has(key)
}

// Put keeps the value in the run store when it differs from what the store
// holds: the real run writes (updates) it, so the entry is reported too.
func (o *overlay) Put(key string, value []byte) error {
	name := FormatName(o.disk.name, o.disk.namespace, key)
	if cur, err := o.Get(key); err == nil && bytes.Equal(cur, value) {
		return nil
	}
	if err := o.dr.Minted("update " + name); err != nil {
		return err
	}
	return o.runPut(name, value)
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
	name := FormatName(o.disk.name, o.disk.namespace, key)
	if err := o.dr.Minted("secret " + name); err != nil {
		return nil, err
	}
	if err := o.runPut(name, v); err != nil {
		return nil, err
	}
	return v, nil
}

// ReadByName reads another Secret's entry (secretRef): the run store first
// (a value an earlier Secret of this run generated), then the store. A miss
// is reported too: the dry run cannot predict that value.
func (o *overlay) ReadByName(filename string) ([]byte, error) {
	if _, err := o.disk.checkName(filename); err != nil {
		return nil, err
	}
	if v, ok := o.runGet(filename); ok {
		return v, nil
	}
	v, err := o.disk.ReadByName(filename)
	if errors.Is(err, os.ErrNotExist) {
		if rerr := o.dr.Minted("secretRef " + filename); rerr != nil {
			return nil, rerr
		}
		return nil, fmt.Errorf("%w (not in the store and not generated earlier in this dry run)", err)
	}
	return v, err
}
