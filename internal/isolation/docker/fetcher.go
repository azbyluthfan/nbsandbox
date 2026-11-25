// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Labels on a run's fetcher and its network. Neither carries LabelRun: the
// run container alone is the run.
const (
	LabelFetcher = "nbsandbox.fetcher"
	LabelNetwork = "nbsandbox.network"
	LabelGateway = "nbsandbox.gateway" // on a run network: the gateway container joined to it
)

// FetcherName is a run's fetcher container; NetworkName its network.
func FetcherName(runID string) string { return "nbsandbox-fetch-" + runID }

// NetworkName is a run's private network.
func NetworkName(runID string) string { return "nbsandbox-run-" + runID }

// fetcherReady is the line the fetcher prints once it serves.
const fetcherReady = "fetcher: serving"

// fetcherStartTimeout bounds the fetcher's start: attaching every source.
const fetcherStartTimeout = 90 * time.Second

// buildFetcherConfig is the fetcher's container: a run's hardening, on the
// run's network only, with its configuration (credentials included) in
// its environment.
func buildFetcherConfig(spec isolation.RunSpec) (*container.Config, *container.HostConfig) {
	init := true
	pids := spec.Limits.PIDs
	cfg := &container.Config{
		Image: spec.Fetcher.Image,
		User:  "1000:1000",
		Env: []string{
			"HOME=/tmp",
			"PYTHONUNBUFFERED=1",
			"NBSANDBOX_FETCHER_LISTEN=grpc://0.0.0.0:" + strconv.Itoa(isolation.FetcherPort),
			"NBSANDBOX_FETCHER_CONFIG_JSON=" + string(spec.Fetcher.Config),
		},
		Labels: map[string]string{LabelFetcher: spec.RunID, LabelSpace: spec.SpaceID},
	}
	host := &container.HostConfig{
		NetworkMode:    container.NetworkMode(NetworkName(spec.RunID)),
		IpcMode:        "private",
		Init:           &init,
		Privileged:     false,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,size=" + strconv.FormatInt(spec.Limits.TmpfsBytes, 10)},
		LogConfig: container.LogConfig{Type: "local", Config: map[string]string{
			"max-size": "1048576", "max-file": "1", "compress": "false",
		}},
		Runtime:    spec.Runtime,
		AutoRemove: false,
		Resources: container.Resources{
			Memory:     spec.Limits.MemoryBytes,
			MemorySwap: spec.Limits.MemoryBytes,
			NanoCPUs:   spec.Limits.NanoCPUs,
			PidsLimit:  &pids,
		},
	}
	if spec.Runtime == "runsc" {
		gvisorPIDs(host, spec.Limits.PIDs)
	}
	return cfg, host
}

// startFetcher gives a run its network, joins the gateway to it, and
// starts the fetcher, returning once it serves. The cleanup it returns
// removes all of it; call it whatever the error.
func (b *Backend) startFetcher(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (func(), error) {
	f := spec.Fetcher
	netName := NetworkName(spec.RunID)
	var fetcherID string
	joined := false
	cleanup := func() {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		if fetcherID != "" {
			if err := b.client.ContainerRemove(cctx, fetcherID); err != nil && !IsNotFound(err) {
				b.log.Warn("remove fetcher", "run_id", spec.RunID, "error", err)
			}
		}
		if joined {
			if err := b.client.NetworkDisconnect(cctx, netName, f.Gateway); err != nil && !IsNotFound(err) {
				b.log.Warn("disconnect the gateway from a run network", "run_id", spec.RunID, "error", err)
			}
		}
		if err := b.client.NetworkRemove(cctx, netName); err != nil && !IsNotFound(err) {
			b.log.Warn("remove run network", "run_id", spec.RunID, "error", err)
		}
	}
	cctx, cancel := freshContext(ctx)
	defer cancel()
	labels := map[string]string{LabelNetwork: spec.RunID, LabelSpace: spec.SpaceID, LabelGateway: f.Gateway}
	if _, err := b.client.NetworkCreate(cctx, netName, labels); err != nil {
		return cleanup, fmt.Errorf("%w: create the run's network: %w", isolation.ErrStart, err)
	}
	if f.Gateway != "" {
		if err := b.client.NetworkConnect(cctx, netName, f.Gateway); err != nil {
			return cleanup, fmt.Errorf("%w: join the gateway (container %q) to the run's network: %w", isolation.ErrStart, f.Gateway, err)
		}
		joined = true
	}
	cfg, host := buildFetcherConfig(spec)
	id, err := b.client.ContainerCreate(cctx, FetcherName(spec.RunID), cfg, host)
	if err != nil {
		return cleanup, fmt.Errorf("%w: create the fetcher: %w", isolation.ErrStart, err)
	}
	fetcherID = id
	if err := b.client.ContainerStart(cctx, id); err != nil {
		return cleanup, fmt.Errorf("%w: start the fetcher: %w", isolation.ErrStart, err)
	}
	if err := b.fetcherReady(ctx, id, out); err != nil {
		return cleanup, fmt.Errorf("%w: %w", isolation.ErrStart, err)
	}
	return cleanup, nil
}

// fetcherReady follows the fetcher's output into the run's log until it
// says it serves. If it ends first, its last lines say why (a source it
// couldn't attach, say).
func (b *Backend) fetcherReady(ctx context.Context, id string, out isolation.LineSink) error {
	lctx, cancel := context.WithTimeout(ctx, fetcherStartTimeout)
	defer cancel()
	rc, err := b.client.ContainerLogs(lctx, id)
	if err != nil {
		return fmt.Errorf("follow the fetcher: %w", err)
	}
	defer func() { _ = rc.Close() }()
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = pw.CloseWithError(err)
	}()
	var last []string
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	for sc.Scan() {
		line := sc.Text()
		out.Line("[fetcher] " + line)
		if strings.HasPrefix(line, fetcherReady) {
			_ = pr.Close()
			return nil
		}
		if last = append(last, line); len(last) > 3 {
			last = last[1:]
		}
	}
	if lctx.Err() != nil && ctx.Err() == nil {
		return errors.New("the fetcher didn't start in time")
	}
	if len(last) == 0 {
		return errors.New("the fetcher stopped before it served")
	}
	return fmt.Errorf("the fetcher stopped before it served: %s", strings.Join(last, " | "))
}

// killFetcher removes what a run left on the network side: its fetcher,
// and its network once the gateway has left it.
func (b *Backend) killFetcher(ctx context.Context, runID string) error {
	var errs []error
	if err := b.client.ContainerRemove(ctx, FetcherName(runID)); err != nil && !IsNotFound(err) {
		errs = append(errs, err)
	}
	nets, err := b.client.NetworkList(ctx, LabelNetwork+"="+runID)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, n := range nets {
		if g := n.Labels[LabelGateway]; g != "" {
			if err := b.client.NetworkDisconnect(ctx, n.ID, g); err != nil && !IsNotFound(err) {
				b.log.Debug("disconnect the gateway from a leftover run network", "run_id", runID, "error", err)
			}
		}
		if err := b.client.NetworkRemove(ctx, n.ID); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
