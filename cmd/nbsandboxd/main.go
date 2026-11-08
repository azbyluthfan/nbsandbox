// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command nbsandboxd is the nbsandbox service.
//
//	nbsandboxd [-config settings.yaml]   run the service (default)
//	nbsandboxd hash-password             print a bcrypt hash for users.yaml
//	nbsandboxd health [-url URL]         exit 0 if nbsandboxd is ready (container healthcheck)
//	nbsandboxd version                   print the version
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "hash-password":
		err = hashPassword(os.Stdin, os.Stdout, os.Stderr)
	case "health":
		err = health(args)
	case "version", "-version", "--version":
		fmt.Println("nbsandboxd", version)
	default:
		err = fmt.Errorf("unknown command %q (commands: serve, hash-password, health, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nbsandboxd:", err)
		os.Exit(1)
	}
}
