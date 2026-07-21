package generator

import "os"

// osRemove is exported via a package-private alias so tests can call
// it without importing "os" at every test file. (Pure cosmetic — keeps
// each test file's imports tight.)
var osRemove = os.Remove

// osReadDir lists the base names of the entries in dir — used by tests that
// assert on which cache files a generator did (and did not) write.
func osReadDir(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names, nil
}
