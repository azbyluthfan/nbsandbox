// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command seed fills a development stack with example spaces, notebooks,
// data, runs and a published function, through the API as the demo users
// (make seed). Safe to run again: a space that already exists (by name, for
// its owner) is left as it is.
//
//	go run ./scripts/seed [-url http://127.0.0.1:8080] [-no-runs]
//
// The demo users' passwords are public (config/users.yaml): this is for
// development stacks only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
)

// file is one file of a seeded space: where it goes and where it comes from
// in the repository.
type file struct{ folder, name, from string }

// space is a seeded space: its files, the runs to start, and the functions
// to publish.
type space struct {
	owner, name string
	files       []file
	runs        []run
	functions   []function
}

type run struct {
	notebook string
	params   map[string]any
}

type function struct{ name, notebook string }

var seeds = []space{
	{
		owner: "alice", name: "Sales analysis",
		files: []file{
			{"", "sales_report.ipynb", "examples/notebooks/sales_report.ipynb"},
			{"", "sales_by_month.ipynb", "examples/notebooks/sales_by_month.ipynb"},
			{"data", "sales.csv", "examples/notebooks/data/sales.csv"},
		},
		runs:      []run{{"sales_report.ipynb", map[string]any{"region": "jakarta", "months": 3}}},
		functions: []function{{"sales-report", "sales_report.ipynb"}},
	},
	{
		owner: "alice", name: "Scratch",
		files: []file{{"", "smoke.ipynb", "tests/workloads/smoke.ipynb"}},
		runs:  []run{{"smoke.ipynb", nil}},
	},
	{
		owner: "bob", name: "Bob's notebooks",
		files: []file{{"", "smoke.ipynb", "tests/workloads/smoke.ipynb"}},
	},
}

// The demo users (config/users.yaml; public, development only).
var passwords = map[string]string{"alice": "alice-dev-password", "bob": "bob-dev-password"}

func main() {
	base := flag.String("url", envOr("NBSANDBOX_URL", "http://127.0.0.1:8080"), "the API's address")
	noRuns := flag.Bool("no-runs", false, "seed files and functions only, no runs")
	flag.Parse()
	if err := seed(context.Background(), &apiclient.Client{BaseURL: *base, HTTP: &http.Client{Timeout: 5 * time.Minute}}, !*noRuns); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func seed(ctx context.Context, c *apiclient.Client, withRuns bool) error {
	for _, s := range seeds {
		who := &apiclient.Identity{Name: s.owner, Password: passwords[s.owner]}
		id, existed, err := ensureSpace(ctx, c, who, s.name)
		if err != nil {
			return fmt.Errorf("%s / %s: %w", s.owner, s.name, err)
		}
		if existed {
			fmt.Printf("%-6s %-18s already there (%s)\n", s.owner, s.name, id)
			continue
		}
		for _, f := range s.files {
			if err := upload(ctx, c, who, id, f); err != nil {
				return fmt.Errorf("%s / %s: upload %s: %w", s.owner, s.name, f.name, err)
			}
		}
		for _, fn := range s.functions {
			if _, err := apiclient.Expect(c.JSONCall(ctx, who, "POST", "/v1/spaces/"+id+"/functions/"+fn.name+"/versions",
				map[string]any{"notebook": fn.notebook}))(http.StatusCreated, "publish "+fn.name); err != nil {
				return err
			}
		}
		status := ""
		if withRuns {
			for _, r := range s.runs {
				started, err := c.StartRun(ctx, who, id, r.notebook, r.params)
				if err != nil {
					return fmt.Errorf("%s / %s: run %s: %w", s.owner, s.name, r.notebook, err)
				}
				done, err := c.WaitRun(ctx, who, id, started.ID, 3*time.Minute)
				if err != nil {
					return fmt.Errorf("%s / %s: run %s: %w", s.owner, s.name, r.notebook, err)
				}
				status += fmt.Sprintf(", %s %s", r.notebook, done.Status)
			}
		}
		fmt.Printf("%-6s %-18s created (%s): %d files%s\n", s.owner, s.name, id, len(s.files), status)
	}
	return nil
}

// ensureSpace returns the owner's space with this name, creating it if
// there is none.
func ensureSpace(ctx context.Context, c *apiclient.Client, who *apiclient.Identity, name string) (string, bool, error) {
	cursor := ""
	for {
		path := "/v1/spaces?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r, err := apiclient.Expect(c.Do(ctx, who, "GET", path, nil, nil))(http.StatusOK, "list spaces")
		if err != nil {
			return "", false, err
		}
		var page struct {
			Items []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(r.Body, &page); err != nil {
			return "", false, err
		}
		for _, sp := range page.Items {
			if sp.Name == name {
				return sp.ID, true, nil
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	id, err := c.CreateSpace(ctx, who, name)
	return id, false, err
}

// upload sends one repository file into a folder of the space.
func upload(ctx context.Context, c *apiclient.Client, who *apiclient.Identity, spaceID string, f file) error {
	b, err := os.ReadFile(filepath.FromSlash(f.from)) //nolint:gosec // repository files named above
	if err != nil {
		return fmt.Errorf("%w (run it from the repository's root)", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, err := mw.CreateFormFile("file", f.name)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	path := "/v1/spaces/" + spaceID + "/files"
	if f.folder != "" {
		path += "?folder=" + url.QueryEscape(f.folder)
	}
	r, err := c.Do(ctx, who, "POST", path, &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if err != nil {
		return err
	}
	if r.Code != http.StatusCreated {
		return errors.New(string(r.Body))
	}
	return nil
}
