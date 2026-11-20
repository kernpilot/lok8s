package kubehz

// credentialOK ports http::credential_ok: it succeeds when no named
// environment variable holds a control character (isControl). Each refused
// variable gets the error of the bash twin, with its name. Each entry point
// calls it before its first request that carries the credential.
//
// The bash twin puts a credential on a curl config line, and a control
// character ends that line: the rest becomes a curl option. The Go client
// refuses a CR or a line feed in a header only as a transport error (the
// caller then says "unreachable"), and it sends a tab. A credential in a JSON
// body (the hcloud token of the token connect) goes out with each control
// character escaped. A common cause is a CR at the end of a value from a file
// with CRLF line ends. An unset or empty variable passes: the caller decides
// on that.
func (c *Context) credentialOK(names ...string) bool {
	ok := true
	for _, name := range names {
		if hasControl(c.getenv(name)) {
			c.errorf("environment variable %s must not contain a control character", name)
			c.echoErr("  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.")
			ok = false
		}
	}
	return ok
}
