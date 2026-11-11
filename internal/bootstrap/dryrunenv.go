package bootstrap

// dryrunenv.go — the dry-run sentinel (LOK8S_BOOTSTRAP_DRYRUN) and its
// scoping. A dispatcher sets it BEFORE any lo code path that can write
// project state runs (the driver's Export writes .registries.json), and
// every lo state writer checks it at its source: the secrets plugin (it
// reads the store and keeps new values in memory), the cert generator (it
// never writes CAROOT), the Lo driver's registry file (computed in memory)
// and the asset resolution (peeked, never ejected).

// SetDryRunEnv scopes LOK8S_BOOTSTRAP_DRYRUN/LOK8S_BOOTSTRAP_ALLOW_GENERATORS
// for the caller's defer. A dispatcher calls it BEFORE driver.Exporter.Export
// so a dry run's Export never writes anything gated on the same sentinel
// (the Lo driver's .registries.json): on both the standalone `lo bootstrap`
// and `lo provision --bootstrap` paths, Export runs before Engine.Apply.
func SetDryRunEnv(dryRun, allowGenerators bool) (restore func()) {
	r1 := setEnvScoped("LOK8S_BOOTSTRAP_DRYRUN", boolEnv(dryRun))
	r2 := setEnvScoped("LOK8S_BOOTSTRAP_ALLOW_GENERATORS", boolEnv(dryRun && allowGenerators))
	return func() { r2(); r1() }
}
