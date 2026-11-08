// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/moby/moby/api/pkg/stdcopy"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// LabelExport marks the one-off containers that read a file out of the
// runner image.
const LabelExport = "io.nbsandbox.export"

// ReadImageFile returns a file of image, at most max bytes. A one-off
// container prints it as base64: no network, no mounts, no capabilities, a
// read-only root and little memory, like a run but shorter. Only the calls
// the socket proxy already allows are used (no archive download).
func ReadImageFile(ctx context.Context, c Client, image, runtime, path string, max int64) ([]byte, error) {
	limits := isolation.Limits{MemoryBytes: 256 << 20, NanoCPUs: 1e9, PIDs: 64, TmpfsBytes: 16 << 20, ShmBytes: 1 << 20,
		LogBytes: 4*max/3 + 64<<10}
	cfg, host := buildContainerConfig(isolation.RunSpec{Image: image, Runtime: runtime, Limits: limits})
	cfg.Entrypoint = []string{"python", "-c", "import base64, sys; sys.stdout.write(base64.b64encode(open(sys.argv[1], 'rb').read()).decode())", path}
	cfg.Cmd = nil
	cfg.Labels = map[string]string{LabelExport: path}
	host.Binds = nil
	var id [6]byte
	_, _ = rand.Read(id[:])
	cid, err := c.ContainerCreate(ctx, "nbsandbox-export-"+hex.EncodeToString(id[:]), cfg, host)
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	defer func() {
		rctx, cancel := freshContext(ctx)
		defer cancel()
		_ = c.ContainerRemove(rctx, cid)
	}()
	if err := c.ContainerStart(ctx, cid); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	waitRes, waitErr := c.ContainerWait(ctx, cid)
	select {
	case <-waitRes:
	case err := <-waitErr:
		return nil, fmt.Errorf("wait: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	st, err := c.ContainerInspect(ctx, cid)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	rc, err := c.ContainerLogs(ctx, cid)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	defer func() { _ = rc.Close() }()
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&limitedWriter{w: &out, n: 4*max/3 + 4}, &limitedWriter{w: &errOut, n: 4 << 10}, rc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read: %w", err)
	}
	if st.ExitCode != 0 {
		return nil, fmt.Errorf("exit %d: %s", st.ExitCode, bytes.TrimSpace(errOut.Bytes()))
	}
	b, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(out.Bytes())))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return b, nil
}

// limitedWriter keeps at most n bytes and reports an error past them.
type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}
