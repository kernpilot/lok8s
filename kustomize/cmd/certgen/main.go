// Command certgen is a minimal, mkcert-compatible certificate generator over
// pkg/certgen — a drop-in for the few `mkcert` invocations the Lo driver makes,
// so the driver needs no `mkcert` binary. It only GENERATES certificates; it
// never touches OS/browser trust stores (that stays `mkcert -install` / `lo
// trust`).
//
//	certgen -CAROOT                              print the CAROOT directory
//	certgen -cert-file C -key-file K <host>...   write a leaf for hosts, signed by
//	                                             the CAROOT CA (created if absent)
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"os"

	"github.com/kernpilot/lok8s/kustomize/pkg/certgen"
)

func main() {
	caroot := flag.Bool("CAROOT", false, "print the CAROOT directory and exit")
	certFile := flag.String("cert-file", "", "output path for the leaf certificate (PEM)")
	keyFile := flag.String("key-file", "", "output path for the leaf private key (PEM)")
	flag.Parse()

	if *caroot {
		dir := certgen.CARoot()
		if dir == "" {
			fail("cannot resolve CAROOT (set $CAROOT)")
		}
		fmt.Println(dir)
		return
	}

	hosts := flag.Args()
	if *certFile == "" || *keyFile == "" || len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "usage: certgen -cert-file C -key-file K <host>...   |   certgen -CAROOT")
		os.Exit(2)
	}

	caCrt, caKey, err := certgen.LoadOrCreateCARoot(rand.Reader)
	if err != nil {
		fail(err)
	}
	leafKey, err := certgen.NewLeafKey(rand.Reader)
	if err != nil {
		fail(err)
	}
	leafCrt, err := certgen.SignLeaf(rand.Reader, caCrt, caKey, leafKey, hosts)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*certFile, leafCrt, 0o644); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*keyFile, leafKey, 0o600); err != nil {
		fail(err)
	}
}

func fail(v any) {
	fmt.Fprintln(os.Stderr, "certgen:", v)
	os.Exit(1)
}
