package cache

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
// only, each name once: the two reads of the generated value give one
// `reuse` line (review round 6, item 7).
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
	if string(raw) != "secret Secret.app.ns.PASSWORD\nreuse Secret.app.ns.PASSWORD\nupdate Secret.app.ns.FROM_ENV\n" {
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
	// Two later processes read the value back (a secretRef, a Get): one
	// `reuse` line, not one per read (review round 6, item 7).
	if strings.Count(string(raw), "reuse Secret.app.ns.PASSWORD\n") != 1 {
		t.Errorf("report = %q, want the reuse reported once", raw)
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

// A no-mint overlay (NewNoMint) keeps a generated or written value in
// memory and records the write as a Refusal with names only: the Secret's
// namespace ("default" when it has none), its name and the key, and
// whether the run would generate the value. A later read of the value in
// the same process finds it and records nothing. An equal Put is no write.
// A secretRef miss is the plain error of a real run, and it is not
// recorded. Minted records "caroot <dir>" and ignores every other line.
// Nothing is written: no store directory, no run store, no report line.
func TestNoMintOverlayRecordsEachWriteAndWritesNothing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	dr := NewNoMint()
	s, err := Open(store, "ns", "app", dr)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("s3cret"), nil }); err != nil || string(v) != "s3cret" {
		t.Fatalf("GetOrCreate = %q, %v", v, err)
	}
	if v, err := s.Get("PASSWORD"); err != nil || string(v) != "s3cret" || !s.Has("PASSWORD") {
		t.Errorf("Get after GetOrCreate = %q, %v", v, err)
	}
	if err := s.Put("PASSWORD", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("FROM_ENV", []byte("value")); err != nil {
		t.Fatal(err)
	}
	other, err := Open(store, "", "ref", dr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.GetOrCreate("TOKEN", func() ([]byte, error) { return []byte("t"), nil }); err != nil {
		t.Fatal(err)
	}
	if v, err := other.ReadByName("Secret.app.ns.PASSWORD"); err != nil || string(v) != "s3cret" {
		t.Errorf("ReadByName of a value of this run = %q, %v", v, err)
	}
	_, err = other.ReadByName("Secret.app.ns.MISSING")
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "dry run") {
		t.Errorf("ReadByName miss = %v, want the plain not-exist error", err)
	}
	for _, line := range []string{"caroot /ca", "reuse caroot /ca", "secret Secret.app.ns.X", "secretRef Secret.app.ns.Y"} {
		if err := dr.Minted(line); err != nil {
			t.Fatalf("Minted(%q) = %v", line, err)
		}
	}
	want := []Refusal{
		{Namespace: "ns", Secret: "app", Key: "PASSWORD", Generate: true},
		{Namespace: "ns", Secret: "app", Key: "FROM_ENV"},
		{Namespace: "default", Secret: "ref", Key: "TOKEN", Generate: true},
		{CARoot: "/ca", Generate: true},
	}
	got := dr.Refused()
	if !slices.Equal(got, want) {
		t.Errorf("Refused = %+v\nwant %+v", got, want)
	}
	got[0].Key = "CHANGED"
	if dr.Refused()[0].Key != "PASSWORD" {
		t.Error("Refused returns the state, not a copy")
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the overlay created the store: %v", err)
	}
	if left := dirListing(t, root); left != "" {
		t.Errorf("the no-mint run wrote %s", left)
	}
}

// Outside a no-mint run nothing is recorded: a nil DryRun and a dry run
// return no refusals, and a dry run reports the CA line as before.
func TestRefusedIsEmptyOutsideANoMintRun(t *testing.T) {
	var none *DryRun
	if got := none.Refused(); got != nil {
		t.Errorf("nil DryRun: Refused = %v", got)
	}
	report := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(report, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dr := DryRunFromEnv(dryRunEnv(report))
	if err := dr.Minted("caroot /ca"); err != nil {
		t.Fatal(err)
	}
	if got := dr.Refused(); len(got) != 0 {
		t.Errorf("dry run: Refused = %v", got)
	}
	if raw, _ := os.ReadFile(report); string(raw) != "caroot /ca\n" {
		t.Errorf("report = %q", raw)
	}
}
