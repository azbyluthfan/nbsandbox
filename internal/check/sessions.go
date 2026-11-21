// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/apiclient"
	"github.com/azbyluthfan/nbsandbox/internal/report"
)

// sessionFindings are the report's findings about interactive sessions,
// all from one check (one session each, opened through the API).
var sessionFindings = []string{"session-network", "session-isolation", "session-memory-limit", "session-read-only"}

// A session is a notebook's live kernel for as long as it lasts: it must be
// held to a run's isolation the whole time.
var sessionsCheck = &Check{Name: "sessions", Findings: sessionFindings,
	run: func(ctx context.Context, e *Env, c *Check) ([]report.Finding, error) {
		sp, err := e.space(ctx, e.A, map[string][]byte{"a.ipynb": workload("smoke.ipynb"), "b.ipynb": workload("smoke.ipynb"),
			"c.ipynb": workload("smoke.ipynb"), "mine.txt": []byte("alice's")})
		if err != nil {
			return nil, err
		}
		if _, err := e.space(ctx, e.B, map[string][]byte{"secret.txt": []byte("SECRET-of-bob-" + randomHex(4))}); err != nil {
			return nil, err
		}
		open := func(notebook, mode string) (*apiclient.Kernel, apiclient.Session, error) {
			s, err := e.Client.OpenSession(ctx, e.A, sp, notebook, mode)
			if err != nil {
				return nil, s, err
			}
			k, err := e.Client.ConnectSession(ctx, e.A, sp, s.ID)
			return k, s, err
		}
		k, s, err := open("a.ipynb", "read-only")
		if errors.Is(err, apiclient.ErrSessionsOff) {
			var out []report.Finding
			for _, id := range sessionFindings {
				out = append(out, c.finding(id, "open an interactive session", "nothing reachable",
					"sessions are turned off on this deployment: nothing to reach", true))
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var out []report.Finding
		f, err := e.sessionNetwork(ctx, c, k)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
		f, err = e.sessionIsolation(ctx, c, k, sp, s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
		f, err = e.sessionReadOnly(ctx, c, k, sp)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
		_ = k.Close()
		_ = e.Client.EndSession(ctx, e.A, sp, s.ID)

		k2, _, err := open("b.ipynb", "read-only")
		if err != nil {
			return out, err
		}
		defer func() { _ = k2.Close() }()
		f, err = e.sessionMemory(ctx, c, k2)
		if err != nil {
			return out, err
		}
		return append(out, f), nil
	}}

func (e *Env) sessionNetwork(ctx context.Context, c *Check, k *apiclient.Kernel) (report.Finding, error) {
	targets := []string{"1.1.1.1:443", "169.254.169.254:80"}
	if u, err := url.Parse(e.Client.BaseURL); err == nil && u.Host != "" {
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = map[string]string{"https": "443"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
		targets = append(targets, host+":"+port) // nbsandbox itself, as clients reach it
	}
	targets = append(targets, e.ExtraTargets...)
	var py strings.Builder
	py.WriteString("import socket\nresults = []\n")
	for _, t := range targets {
		host, port, _ := strings.Cut(t, ":")
		fmt.Fprintf(&py, "try:\n    socket.create_connection((%q, %s), timeout=3).close(); results.append(%q + ':open')\nexcept OSError:\n    results.append(%q + ':blocked')\n",
			host, port, t, t)
	}
	py.WriteString("try:\n    socket.getaddrinfo('example.com', 443); results.append('dns:resolves')\nexcept OSError:\n    results.append('dns:none')\nprint(' '.join(results))")
	got, err := k.Run(ctx, py.String(), 2*time.Minute)
	if err != nil {
		return report.Finding{}, err
	}
	got = strings.TrimSpace(got)
	ok := got != "" && !strings.Contains(got, ":open") && !strings.Contains(got, "dns:resolves")
	return c.finding("session-network", "a session's kernel connects to the internet, the cloud metadata address, nbsandbox and the cluster, and resolves a name",
		"every connection and lookup fails", got, ok), nil
}

func (e *Env) sessionIsolation(ctx context.Context, c *Check, k *apiclient.Kernel, sp, id string) (report.Finding, error) {
	got, err := k.Run(ctx, `import os
cap = [l.split()[1] for l in open('/proc/self/status') if l.startswith('CapEff')][0]
try:
    open('/usr/x', 'w'); rofs = 'writable'
except OSError:
    rofs = 'read-only'
found = []
for root, dirs, files in os.walk('/'):
    if root == '/':
        dirs[:] = [d for d in dirs if d not in ('proc', 'sys', 'dev')]
    found += [os.path.join(root, f) for f in files if f == 'secret.txt']
print(os.getuid(), cap, rofs, 'mine.txt' in os.listdir('/space'), found)`, 3*time.Minute)
	if err != nil {
		return report.Finding{}, err
	}
	got = strings.TrimSpace(got)
	// And another user can't reach it.
	r, err := e.Client.Do(ctx, e.B, "GET", "/v1/spaces/"+sp+"/sessions/"+id, nil, nil)
	if err != nil {
		return report.Finding{}, err
	}
	got += fmt.Sprintf("; bob asking for it: %d", r.Code)
	ok := strings.HasPrefix(got, "1000 0000000000000000 read-only True []") && r.Code == http.StatusNotFound
	return c.finding("session-isolation", "a session looks at its user, capabilities, root filesystem, /space and other spaces' files; another user asks for it",
		"uid 1000, no capabilities, a read-only system, only its own space's files; 404 for the other user", got, ok), nil
}

func (e *Env) sessionReadOnly(ctx context.Context, c *Check, k *apiclient.Kernel, sp string) (report.Finding, error) {
	got, err := k.Run(ctx, "import os\nfor op in (lambda: open('mine.txt','w').write('changed'), lambda: os.remove('mine.txt'), lambda: open('new.txt','w')):\n"+
		"    try:\n        op(); print('wrote')\n    except OSError as e:\n        print(type(e).__name__)", time.Minute)
	if err != nil {
		return report.Finding{}, err
	}
	r, err := e.Client.Do(ctx, e.A, "GET", "/v1/spaces/"+sp+"/files/mine.txt", nil, nil)
	if err != nil {
		return report.Finding{}, err
	}
	got = fmt.Sprintf("%s; mine.txt: %q", strings.Join(strings.Fields(got), " "), r.Body)
	ok := !strings.Contains(got, "wrote") && string(r.Body) == "alice's"
	return c.finding("session-read-only", "a read-only session changes, deletes and creates files in /space",
		"every write fails; the space is unchanged", got, ok), nil
}

func (e *Env) sessionMemory(ctx context.Context, c *Check, k *apiclient.Kernel) (report.Finding, error) {
	if _, err := k.Send("shell", "execute_request", map[string]any{"code": "x = []\nwhile True: x.append(bytearray(64 << 20))"}); err != nil {
		return report.Finding{}, err
	}
	start := time.Now()
	m, err := k.Next(ctx, 3*time.Minute, func(m apiclient.KernelMsg) bool {
		st := m.Content["status"]
		return m.Channel == "bridge" && (st == "dead" || st == "restarting" || st == "ended")
	})
	if err != nil {
		return report.Finding{}, err
	}
	var got string
	ok := false
	if m.Content["status"] == "ended" {
		got = fmt.Sprintf("the session ended after %.0fs (%v)", time.Since(start).Seconds(), m.Content["reason"])
		ok = e.Kubernetes // the kubelet kills the whole container there
	} else {
		if _, err := k.Next(ctx, 3*time.Minute, func(m apiclient.KernelMsg) bool { return m.Channel == "bridge" && m.Content["status"] == "ready" }); err != nil {
			return report.Finding{}, err
		}
		out, err := k.Run(ctx, "print('alive')", time.Minute)
		if err != nil {
			return report.Finding{}, err
		}
		got = fmt.Sprintf("kernel %v, ready again after %.0fs; the next cell printed %q", m.Content["status"], time.Since(start).Seconds(), strings.TrimSpace(out))
		ok = strings.TrimSpace(out) == "alive"
	}
	r, err := e.Client.Do(ctx, nil, "GET", "/readyz", nil, nil)
	if err == nil {
		got += fmt.Sprintf("; nbsandbox ready %d", r.Code)
		ok = ok && r.Code == http.StatusOK
	}
	return c.finding("session-memory-limit", "a session's cell allocates memory without bound",
		"the kernel is killed at the limit (Docker: started again, the session goes on; Kubernetes: the session ends); nbsandbox goes on", got, ok), nil
}
