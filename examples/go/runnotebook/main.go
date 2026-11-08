// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command runnotebook is another app running a notebook in nbsandbox: upload a
// notebook and its data, start a run with parameters, follow its result over
// Server-Sent Events (item by item, for a notebook that streams its output),
// and print what it returned. Standard library only.
//
//	export NBSANDBOX_URL=http://127.0.0.1:8080
//	export NBSANDBOX_TOKEN=...            # a bearer token issued for nbsandbox
//	# or: export NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
//	go run ./examples/go/runnotebook -region bandung -months 4
//	go run ./examples/go/runnotebook -stream          # items as they are made
//
// A backend acting for a signed-in user exchanges the user's token for one
// issued to nbsandbox first (Entra ID On-Behalf-Of, or RFC 8693 token
// exchange); see examples/python/run_notebook.py and docs/auth.md.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type client struct {
	base       string
	token      string
	user, pass string
	http       *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, contentType string, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.SetBasicAuth(c.user, c.pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		// 401: credentials; 403: the token lacks a scope (see WWW-Authenticate).
		return nil, fmt.Errorf("%s %s: %d %s: %s", method, path, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	return resp, nil
}

func (c *client) json(ctx context.Context, method, path string, in, out any, hdr map[string]string) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, body, ct, hdr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// upload sends files (all or nothing) into folder ("" = the top level).
func (c *client) upload(ctx context.Context, space, folder string, paths ...string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // the example's own files
		if err != nil {
			return err
		}
		w, err := mw.CreateFormFile("file", filepath.Base(p))
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	q := ""
	if folder != "" {
		q = "?folder=" + url.QueryEscape(folder)
	}
	resp, err := c.do(ctx, "POST", "/v1/spaces/"+space+"/files"+q, &buf, mw.FormDataContentType(), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// result is what a finished run gives a caller: output, or error. A
// streamed run's output is the list of its items; one that failed part-way
// also has the items so far (partial).
type result struct {
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	Items   *int            `json:"items"`
	Partial bool            `json:"partial"`
	Error   *struct {
		Kind, Message string
		Retryable     bool
	} `json:"error"`
}

// item is one item of a streamed output, as it arrives.
type item struct {
	Index int             `json:"index"`
	Total *int            `json:"total"`
	Item  json.RawMessage `json:"item"`
}

// follow reads the run's result stream (GET .../result as Server-Sent
// Events): each item of a streamed output as soon as it is made, then the
// result. onItem sees every item exactly once: if the connection drops, it
// reconnects with Last-Event-ID (the last item it has), and the server
// continues after it. A notebook that returns one value sends just the
// result.
func (c *client) follow(ctx context.Context, space, run string, onItem func(item)) (result, error) {
	last := 0
	for attempt := 0; attempt < 5; attempt++ {
		hdr := map[string]string{"Accept": "text/event-stream"}
		if last > 0 {
			hdr["Last-Event-ID"] = strconv.Itoa(last)
		}
		resp, err := c.do(ctx, "GET", "/v1/spaces/"+space+"/runs/"+run+"/result", nil, "", hdr)
		if err != nil {
			return result{}, err
		}
		res, done, err := readResultStream(resp.Body, func(it item) {
			if it.Index > last { // never twice
				last = it.Index
				onItem(it)
			}
		})
		_ = resp.Body.Close()
		if done {
			return res, nil
		}
		fmt.Printf("  stream interrupted (%v); resuming after item %d\n", err, last)
		time.Sleep(time.Second)
	}
	return result{}, errors.New("the result stream kept dropping")
}

// readResultStream reads events until the result (done) or the stream ends.
func readResultStream(body io.Reader, onItem func(item)) (result, bool, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		case line == "": // end of one event
			switch event {
			case "item":
				var it item
				if err := json.Unmarshal([]byte(data), &it); err == nil {
					onItem(it)
				}
			case "result":
				var res result
				err := json.Unmarshal([]byte(data), &res)
				return res, err == nil, err
			}
			event, data = "", ""
		}
	}
	if err := sc.Err(); err != nil {
		return result{}, false, err
	}
	return result{}, false, errors.New("the stream ended before the result")
}

func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	region := flag.String("region", "jakarta", "jakarta, bandung or surabaya")
	months := flag.Int("months", 3, "how many recent months to summarise")
	stream := flag.Bool("stream", false, "run sales_by_month.ipynb, which streams one item per month, instead of sales_report.ipynb")
	dir := flag.String("notebooks", "examples/notebooks", "folder with the notebooks and data/")
	flag.Parse()
	notebook := "sales_report.ipynb"
	if *stream {
		notebook = "sales_by_month.ipynb"
	}

	c := &client{
		base:  strings.TrimRight(envOr("NBSANDBOX_URL", "http://127.0.0.1:8080"), "/"),
		token: os.Getenv("NBSANDBOX_TOKEN"), user: os.Getenv("NBSANDBOX_USER"), pass: os.Getenv("NBSANDBOX_PASSWORD"),
		http: &http.Client{}, // no overall timeout: the event stream stays open; contexts bound each call
	}
	if c.token == "" && c.user == "" {
		log.Fatal("set NBSANDBOX_TOKEN, or NBSANDBOX_USER and NBSANDBOX_PASSWORD")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var sp struct {
		ID string `json:"id"`
	}
	must(c.json(ctx, "POST", "/v1/spaces", map[string]string{"name": "example " + time.Now().Format("2006-01-02 15:04:05")}, &sp, nil))
	fmt.Println("space", sp.ID)
	must(c.upload(ctx, sp.ID, "", filepath.Join(*dir, notebook)))
	must(c.upload(ctx, sp.ID, "data", filepath.Join(*dir, "data", "sales.csv")))

	var run struct{ ID, Status string }
	must(c.json(ctx, "POST", "/v1/spaces/"+sp.ID+"/runs",
		map[string]any{"notebook": notebook, "parameters": map[string]any{"region": *region, "months": *months}},
		&run, map[string]string{"Idempotency-Key": newKey()})) // a retry with the same key returns this run
	fmt.Printf("run %s: %s\n", run.ID, run.Status)

	res, err := c.follow(ctx, sp.ID, run.ID, func(it item) {
		// A chart would add a point here, as each month arrives.
		of := ""
		if it.Total != nil {
			of = fmt.Sprintf(" of %d", *it.Total)
		}
		fmt.Printf("  item %d%s: %s\n", it.Index, of, it.Item)
	})
	must(err)
	if res.Status != "succeeded" {
		if res.Partial {
			fmt.Printf("items before it failed: %s\n", res.Output) // kept; run again for the rest
		}
		log.Fatalf("run %s: %s: %s (retryable: %v)", res.Status, res.Error.Kind, res.Error.Message, res.Error.Retryable)
	}
	fmt.Printf("output: %s\n", res.Output) // a streamed run: the list of the items above
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
