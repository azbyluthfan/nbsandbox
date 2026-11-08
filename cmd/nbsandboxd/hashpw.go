// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
)

// hashPassword reads a password (without echo when stdin is a terminal) and
// prints its bcrypt hash for users.yaml.
func hashPassword(in *os.File, out, prompt io.Writer) error {
	var pw string
	if term.IsTerminal(int(in.Fd())) {
		_, _ = fmt.Fprint(prompt, "Password: ")
		b, err := term.ReadPassword(int(in.Fd()))
		_, _ = fmt.Fprintln(prompt)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(prompt, "Repeat:   ")
		b2, err := term.ReadPassword(int(in.Fd()))
		_, _ = fmt.Fprintln(prompt)
		if err != nil {
			return err
		}
		if string(b) != string(b2) {
			return errors.New("passwords do not match")
		}
		pw = string(b)
	} else {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, h)
	return nil
}
