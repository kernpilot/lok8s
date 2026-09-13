//go:build inprocess

package render

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two spellings of one directory share one overlay file (renderenv keys it
// on cleanDir), so they must share one lock too. Keyed on the raw path,
// two renders of the same directory ran at once and read each other's
// variables.
func TestLockDirKeysOnTheCleanedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for name, spelling := range map[string]string{
		"trailing dot": dir + string(os.PathSeparator) + ".",
		"symlink":      link,
	} {
		t.Run(name, func(t *testing.T) {
			unlock := lockDir(dir)
			var once sync.Once
			release := func() { once.Do(unlock) }
			// A failed assertion must not keep the lock for the next case.
			t.Cleanup(release)
			acquired := make(chan struct{})
			go func() {
				defer close(acquired)
				lockDir(spelling)()
			}()
			select {
			case <-acquired:
				t.Fatalf("%q took a lock of its own instead of waiting for %q", spelling, dir)
			case <-time.After(200 * time.Millisecond):
			}
			release()
			select {
			case <-acquired:
			case <-time.After(5 * time.Second):
				t.Fatal("the lock was not handed over")
			}
		})
	}
}
