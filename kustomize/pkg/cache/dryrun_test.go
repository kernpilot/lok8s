package cache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dryRunEnv(report string) func(string) (string, bool) {
	return dryRunEnvStore(report, "")
}

func dryRunEnvStore(report, store string) func(string) (string, bool) {
	vals := map[string]string{DryRunEnv: "1", DryRunReportEnv: report}
	if store != "" {
		vals[DryRunStoreEnv] = store
	}
	return func(k string) (string, bool) { v, ok := vals[k]; return v, ok }
}

func TestDryRunFromEnvIsNilOutsideADryRun(t *testing.T) {
	if DryRunFromEnv(func(string) (string, bool) { return "", false }) != nil {
		t.Error("a normal run got the dry-run state")
	}
	if DryRunFromEnv(func(k string) (string, bool) { return "0", k == DryRunEnv }) != nil {
		t.Error("LOK8S_BOOTSTRAP_DRYRUN=0 counts as a dry run")
	}
}

// The overlay reads the store, generates a missing value in memory and
// writes nothing: no directory, no file. A generated value, a read of it
// from the run store (reuse: the reader is not repeatable either) and an
// update (Put of a value the store does not hold) are reported by name
// only.
func TestOverlayWritesNothingAndReportsWhatItGenerated(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	if err := os.WriteFile(report, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	dr := DryRunFromEnv(dryRunEnv(report))
	s, err := Open(store, "ns", "app", dr)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("s3cret"), nil })
	if err != nil || string(v) != "s3cret" {
		t.Fatalf("GetOrCreate = %q, %v", v, err)
	}
	// Same process, same value (a later generator reads it back).
	if v, _ := s.Get("PASSWORD"); string(v) != "s3cret" || !s.Has("PASSWORD") {
		t.Errorf("Get after GetOrCreate = %q", v)
	}
	other, _ := Open(store, "ns", "ref", dr)
	if v, err := other.ReadByName("Secret.app.ns.PASSWORD"); err != nil || string(v) != "s3cret" {
		t.Errorf("ReadByName across stores = %q, %v", v, err)
	}
	if err := s.Put("FROM_ENV", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the overlay created the store: %v", err)
	}
	raw, _ := os.ReadFile(report)
	if string(raw) != "secret Secret.app.ns.PASSWORD\nreuse Secret.app.ns.PASSWORD\nreuse Secret.app.ns.PASSWORD\nupdate Secret.app.ns.FROM_ENV\n" {
		t.Errorf("report = %q", raw)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Error("the report carries the value")
	}
}

// A stored value is read and not reported; the store is untouched.
func TestOverlayReadsTheStore(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	_ = os.WriteFile(report, nil, 0o600)
	store := filepath.Join(root, "store")
	c, _ := New(store, "ns", "app")
	_ = c.Put("PASSWORD", []byte("stored"))
	s, _ := Open(store, "ns", "app", DryRunFromEnv(dryRunEnv(report)))
	v, err := s.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("new"), nil })
	if err != nil || string(v) != "stored" {
		t.Fatalf("GetOrCreate = %q, %v", v, err)
	}
	if raw, _ := os.ReadFile(report); len(raw) != 0 {
		t.Errorf("a stored value was reported: %q", raw)
	}
}

// A secretRef to an entry the store lacks is an error that says why, and
// it is reported (the real run may generate it first).
func TestOverlayReadByNameMissIsReported(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	_ = os.WriteFile(report, nil, 0o600)
	s, _ := Open(filepath.Join(root, "store"), "ns", "ref", DryRunFromEnv(dryRunEnv(report)))
	_, err := s.ReadByName("Secret.app.ns.PASSWORD")
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "not generated earlier in this dry run") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(report); string(raw) != "secretRef Secret.app.ns.PASSWORD\n" {
		t.Errorf("report = %q", raw)
	}
	if _, err := s.ReadByName("../escape"); err == nil {
		t.Error("the overlay skipped the traversal check")
	}
}

// The plugin never creates the report file: lo does. A report that cannot
// be opened is an error (fail closed).
func TestDryRunReportMustExist(t *testing.T) {
	report := filepath.Join(t.TempDir(), "missing")
	s, _ := Open(t.TempDir(), "ns", "app", DryRunFromEnv(dryRunEnv(report)))
	if _, err := s.GetOrCreate("K", func() ([]byte, error) { return []byte("v"), nil }); err == nil {
		t.Fatal("generated without a report file")
	}
	if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
		t.Error("the plugin created the report file")
	}
}

// A miss in an EXISTING store directory (the production case: the store is
// there, one value is new) writes nothing into it, generate or update.
func TestOverlayMissInAnExistingStoreWritesNothing(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	_ = os.WriteFile(report, nil, 0o600)
	store := filepath.Join(root, "store")
	c, _ := New(store, "ns", "app")
	_ = c.Put("OLD", []byte("old"))
	before := dirListing(t, store)
	s, _ := Open(store, "ns", "app", DryRunFromEnv(dryRunEnvStore(report, filepath.Join(root, "run"))))
	if _, err := s.GetOrCreate("NEW", func() ([]byte, error) { return []byte("v"), nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("OLD", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if after := dirListing(t, store); after != before {
		t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
	}
	if v, _ := os.ReadFile(filepath.Join(store, FormatName("app", "ns", "OLD"))); string(v) != "old" {
		t.Errorf("OLD = %q, want the stored value untouched", v)
	}
}

// The run store carries a value from one plugin process (one DryRun) to the
// next: a later Secret of the same run reads what an earlier one generated
// or updated. It is 0700/0600.
func TestRunStoreCarriesValuesAcrossPluginProcesses(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	_ = os.WriteFile(report, nil, 0o600)
	store, run := filepath.Join(root, "store"), filepath.Join(root, "run")
	first, _ := Open(store, "ns", "app", DryRunFromEnv(dryRunEnvStore(report, run)))
	v1, err := first.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("gen-1"), nil })
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Put("FROM_ENV", []byte("env-1"))
	second, _ := Open(store, "ns", "ref", DryRunFromEnv(dryRunEnvStore(report, run)))
	if v, err := second.ReadByName(FormatName("app", "ns", "PASSWORD")); err != nil || string(v) != string(v1) {
		t.Errorf("secretRef in a later process = %q, %v", v, err)
	}
	again, _ := Open(store, "ns", "app", DryRunFromEnv(dryRunEnvStore(report, run)))
	if v, _ := again.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("gen-2"), nil }); string(v) != "gen-1" {
		t.Errorf("a later render regenerated the value: %q", v)
	}
	if v, _ := again.Get("FROM_ENV"); string(v) != "env-1" {
		t.Errorf("a later process lost the update: %q", v)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the real store was created: %v", err)
	}
	_ = filepath.Walk(run, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if p != run && info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	raw, _ := os.ReadFile(report)
	if strings.Count(string(raw), "secret Secret.app.ns.PASSWORD") != 1 {
		t.Errorf("report = %q, want the generation reported once", raw)
	}
}

func dirListing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		info, _ := e.Info()
		b.WriteString(e.Name() + " " + info.ModTime().String() + "\n")
	}
	return b.String()
}
