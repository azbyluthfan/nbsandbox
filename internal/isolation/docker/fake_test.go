// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
)

// noContainers gives a fake the container methods it doesn't care about.
type noContainers struct{}

func (noContainers) ContainerCreate(context.Context, string, *container.Config, *container.HostConfig) (string, error) {
	return "", errors.New("not implemented")
}
func (noContainers) ContainerStart(context.Context, string) error {
	return errors.New("not implemented")
}
func (noContainers) ContainerWait(context.Context, string) (<-chan container.WaitResponse, <-chan error) {
	return nil, nil
}
func (noContainers) ContainerLogs(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (noContainers) ContainerStop(context.Context, string, time.Duration) error { return nil }
func (noContainers) ContainerInspect(context.Context, string) (ContainerState, error) {
	return ContainerState{}, errors.New("not implemented")
}
func (noContainers) ContainerRemove(context.Context, string) error { return nil }
func (noContainers) ContainerList(context.Context, string) ([]ContainerSummary, error) {
	return nil, nil
}

// fakeDocker scripts one container's life for backend tests.
type fakeDocker struct {
	mu sync.Mutex

	createErr, startErr error
	logs                []byte // multiplexed stdout/stderr
	exit                chan int
	waitErr             chan error
	inspect             ContainerState
	inspectErr          error
	stopExits           bool // a stop makes the container exit with 143
	containers          []ContainerSummary

	calls         []string
	cancelledCtxs []string // calls made with an already-cancelled context
	created       struct {
		name string
		cfg  *container.Config
		host *container.HostConfig
	}
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{exit: make(chan int, 1), waitErr: make(chan error, 1)}
}

func (f *fakeDocker) record(ctx context.Context, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if ctx.Err() != nil {
		f.cancelledCtxs = append(f.cancelledCtxs, call)
	}
}

func (f *fakeDocker) Ping(ctx context.Context) error { f.record(ctx, "ping"); return nil }
func (f *fakeDocker) Info(ctx context.Context) (Info, error) {
	f.record(ctx, "info")
	return Info{}, nil
}
func (f *fakeDocker) ImageInspect(ctx context.Context, _ string) (Image, error) {
	f.record(ctx, "image")
	return Image{}, nil
}
func (f *fakeDocker) Close() error { return nil }

func (f *fakeDocker) ContainerCreate(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig) (string, error) {
	f.record(ctx, "create")
	f.created.name, f.created.cfg, f.created.host = name, cfg, host
	if f.createErr != nil {
		return "", f.createErr
	}
	return "cid123", nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, _ string) error {
	f.record(ctx, "start")
	return f.startErr
}

func (f *fakeDocker) ContainerWait(ctx context.Context, _ string) (<-chan container.WaitResponse, <-chan error) {
	f.record(ctx, "wait")
	res := make(chan container.WaitResponse, 1)
	errc := make(chan error, 1)
	go func() {
		select {
		case code := <-f.exit:
			res <- container.WaitResponse{StatusCode: int64(code)}
		case err := <-f.waitErr:
			errc <- err
		case <-ctx.Done():
			errc <- ctx.Err()
		}
	}()
	return res, errc
}

func (f *fakeDocker) ContainerLogs(ctx context.Context, _ string) (io.ReadCloser, error) {
	f.record(ctx, "logs")
	return io.NopCloser(bytes.NewReader(f.logs)), nil
}

func (f *fakeDocker) ContainerStop(ctx context.Context, _ string, _ time.Duration) error {
	f.record(ctx, "stop")
	if f.stopExits {
		f.mu.Lock()
		f.inspect.ExitCode = 143
		f.mu.Unlock()
		select {
		case f.exit <- 143:
		default:
		}
	}
	return nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, _ string) (ContainerState, error) {
	f.record(ctx, "inspect")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspect, f.inspectErr
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, _ string) error {
	f.record(ctx, "remove")
	return nil
}

func (f *fakeDocker) ContainerList(ctx context.Context, _ string) ([]ContainerSummary, error) {
	f.record(ctx, "list")
	return f.containers, nil
}

func (f *fakeDocker) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// multiplex builds Docker's multiplexed log stream: frames of an 8-byte
// header (stream id, 3 zero bytes, big-endian length) and the payload.
func multiplex(stdout, stderr string) []byte {
	var buf bytes.Buffer
	frame := func(stream byte, s string) {
		if s == "" {
			return
		}
		hdr := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
		buf.Write(hdr)
		buf.WriteString(s)
	}
	frame(1, stdout)
	frame(2, stderr)
	return buf.Bytes()
}
