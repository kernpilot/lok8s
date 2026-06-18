package generator

import "os"

// osRemove is exported via a package-private alias so tests can call
// it without importing "os" at every test file. (Pure cosmetic — keeps
// each test file's imports tight.)
var osRemove = os.Remove
