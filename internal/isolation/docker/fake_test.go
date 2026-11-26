// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
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
func (noContainers) ContainerAttach(context.Context, string) (Attached, error) {
	return Attached{}, errors.New("not implemented")
}
func (noContainers) NetworkCreate(context.Context, string, map[string]string) (string, error) {
	return "", errors.New("not implemented")
}
func (noContainers) NetworkConnect(context.Context, string, string) error        { return nil }
func (noContainers) ContainerIP(context.Context, string, string) (string, error) { return "", nil }
func (noContainers) NetworkDisconnect(context.Context, string, string) error     { return nil }
func (noContainers) NetworkRemove(context.Context, string) error                 { return nil }
func (noContainers) NetworkList(context.Context, string) ([]NetworkSummary, error) {
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
	attach              func(id string) (Attached, error)
	containers          []ContainerSummary
	fetchers            []ContainerSummary // listed for LabelFetcher
	networks            []NetworkSummary
	fetcherLogs         []byte // the fetcher container's output
	fetcherCreated      struct {
		cfg  *container.Config
		host *container.HostConfig
	}

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

// fetcherID is the id the fake gives a fetcher container: calls on it are
// recorded as "fetcher-<call>".
const fetcherID = "fid456"

func (f *fakeDocker) ContainerCreate(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig) (string, error) {
	if strings.HasPrefix(name, "nbsandbox-fetch-") {
		f.record(ctx, "fetcher-create")
		f.fetcherCreated.cfg, f.fetcherCreated.host = cfg, host
		return fetcherID, nil
	}
	f.record(ctx, "create")
	f.created.name, f.created.cfg, f.created.host = name, cfg, host
	if f.createErr != nil {
		return "", f.createErr
	}
	return "cid123", nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, id string) error {
	if id == fetcherID {
		f.record(ctx, "fetcher-start")
		return nil
	}
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

func (f *fakeDocker) ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	if id == fetcherID {
		f.record(ctx, "fetcher-logs")
		return io.NopCloser(bytes.NewReader(f.fetcherLogs)), nil
	}
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

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string) error {
	if id == fetcherID || strings.HasPrefix(id, "nbsandbox-fetch-") {
		f.record(ctx, "fetcher-remove")
		return nil
	}
	f.record(ctx, "remove")
	return nil
}

func (f *fakeDocker) ContainerList(ctx context.Context, label string) ([]ContainerSummary, error) {
	if label == LabelFetcher {
		f.record(ctx, "list-fetchers")
		return f.fetchers, nil
	}
	f.record(ctx, "list")
	return f.containers, nil
}

func (f *fakeDocker) NetworkCreate(ctx context.Context, name string, _ map[string]string) (string, error) {
	f.record(ctx, "network-create "+name)
	return "nid", nil
}
func (f *fakeDocker) NetworkConnect(ctx context.Context, network, ctr string) error {
	f.record(ctx, "network-connect "+network+" "+ctr)
	return nil
}
func (f *fakeDocker) ContainerIP(ctx context.Context, ctr, network string) (string, error) {
	f.record(ctx, "ip "+ctr)
	if ctr == fetcherID {
		return "10.5.0.3", nil
	}
	return "10.5.0.2", nil // the gateway
}
func (f *fakeDocker) NetworkDisconnect(ctx context.Context, network, ctr string) error {
	f.record(ctx, "network-disconnect "+network+" "+ctr)
	return nil
}
func (f *fakeDocker) NetworkRemove(ctx context.Context, network string) error {
	f.record(ctx, "network-remove "+network)
	return nil
}
func (f *fakeDocker) NetworkList(ctx context.Context, label string) ([]NetworkSummary, error) {
	f.record(ctx, "network-list "+label)
	var out []NetworkSummary
	for _, n := range f.networks {
		if k, v, ok := strings.Cut(label, "="); !ok || n.Labels[k] == v {
			if _, has := n.Labels[k]; has {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func (f *fakeDocker) ContainerAttach(ctx context.Context, id string) (Attached, error) {
	f.record(ctx, "attach")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attach == nil {
		return Attached{}, errors.New("not attachable")
	}
	return f.attach(id)
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
