package image

// names.go: the one place the cache registry's docker name comes from.
// Clean removes it, and the `lo image clean` confirmation lists it.

// CacheRegistry is the name of the cache registry's container and volume
// on the given docker network ("" is the default network, lok8s).
func CacheRegistry(network string) string {
	if network == "" {
		network = "lok8s"
	}
	return network + "-registry-cache"
}
