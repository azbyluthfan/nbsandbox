// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// Client is the narrow slice of the Docker API nbsandboxd uses. Keeping it small
// lets every caller be unit-tested with a fake, and isolates the rest of the
// code from Docker client module changes.
type Client interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (Info, error)
	ImageInspect(ctx context.Context, ref string) (Image, error)
	Close() error

	// Container lifecycle. Exactly the calls the socket proxy
	// allows.
	ContainerCreate(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig) (string, error)
	ContainerStart(ctx context.Context, id string) error
	// ContainerWait returns when the container is no longer running.
	ContainerWait(ctx context.Context, id string) (<-chan container.WaitResponse, <-chan error)
	// ContainerLogs follows stdout and stderr (multiplexed) from the start.
	ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error)
	ContainerStop(ctx context.Context, id string, timeout time.Duration) error
	ContainerInspect(ctx context.Context, id string) (ContainerState, error)
	// ContainerUsage reads a container's resource use now (one reading).
	ContainerUsage(ctx context.Context, id string) (isolation.Usage, error)
	ContainerRemove(ctx context.Context, id string) error
	// ContainerList lists containers (running or not) carrying label.
	ContainerList(ctx context.Context, label string) ([]ContainerSummary, error)
	// ContainerAttach attaches to a container's stdin, stdout and stderr
	// (an interactive session's): stdout and stderr come multiplexed.
	ContainerAttach(ctx context.Context, id string) (Attached, error)

	// Run networks: a run with a fetcher gets a private network of its
	// own (internal: no way out but the gateway).
	NetworkCreate(ctx context.Context, name string, labels map[string]string) (string, error)
	NetworkConnect(ctx context.Context, network, container string) error
	// ContainerIP is a container's address on a network ("" if it has none).
	ContainerIP(ctx context.Context, container, network string) (string, error)
	NetworkDisconnect(ctx context.Context, network, container string) error
	NetworkRemove(ctx context.Context, network string) error
	// NetworkList lists networks carrying label.
	NetworkList(ctx context.Context, label string) ([]NetworkSummary, error)
}

// NetworkSummary is one entry of a network list.
type NetworkSummary struct {
	ID, Name string
	Labels   map[string]string
}

// Attached is an attach's connection: write to stdin, read the multiplexed
// stdout and stderr; Close ends the attach, not the container.
type Attached struct {
	Conn   io.WriteCloser
	Reader io.Reader
}

// ContainerState is what nbsandboxd needs from a container inspect.
type ContainerState struct {
	Running    bool
	ExitCode   int
	OOMKilled  bool
	StartedAt  time.Time
	FinishedAt time.Time
	Labels     map[string]string
}

// ContainerSummary is one entry of a container list.
type ContainerSummary struct {
	ID      string
	Labels  map[string]string
	Running bool
}

// Info is what nbsandboxd needs from `docker info`.
type Info = isolation.Info

// Image is what nbsandboxd needs from an image inspect.
type Image = isolation.Image

// NewClient connects to host ("" = DOCKER_HOST, then the default socket).
// The client negotiates the API version with the daemon or proxy by default.
func NewClient(host string) (Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &mobyClient{c: c}, nil
}

type mobyClient struct{ c *client.Client }

func (m *mobyClient) Ping(ctx context.Context) error {
	_, err := m.c.Ping(ctx, client.PingOptions{})
	return err
}

func (m *mobyClient) Info(ctx context.Context) (Info, error) {
	res, err := m.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Info{}, err
	}
	i := res.Info
	out := Info{
		ServerVersion:   i.ServerVersion,
		OperatingSystem: i.OperatingSystem,
		KernelVersion:   i.KernelVersion,
		CgroupVersion:   i.CgroupVersion,
	}
	for name := range i.Runtimes {
		out.Runtimes = append(out.Runtimes, name)
	}
	sort.Strings(out.Runtimes)
	return out, nil
}

func (m *mobyClient) ImageInspect(ctx context.Context, ref string) (Image, error) {
	res, err := m.c.ImageInspect(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	img := Image{ID: res.ID, RepoDigests: res.RepoDigests}
	if res.Config != nil {
		img.Labels = res.Config.Labels
	}
	return img, nil
}

func (m *mobyClient) Close() error { return m.c.Close() }

func (m *mobyClient) ContainerCreate(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig) (string, error) {
	res, err := m.c.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: host})
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

func (m *mobyClient) ContainerStart(ctx context.Context, id string) error {
	_, err := m.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (m *mobyClient) ContainerWait(ctx context.Context, id string) (<-chan container.WaitResponse, <-chan error) {
	res := m.c.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	return res.Result, res.Error
}

func (m *mobyClient) ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	return m.c.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
}

func (m *mobyClient) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Round(time.Second) / time.Second)
	_, err := m.c.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &secs})
	return err
}

func (m *mobyClient) ContainerUsage(ctx context.Context, id string) (isolation.Usage, error) {
	res, err := m.c.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return isolation.Usage{}, err
	}
	defer func() { _ = res.Body.Close() }()
	var st container.StatsResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&st); err != nil {
		return isolation.Usage{}, err
	}
	return usageOf(st), nil
}

// usageOf reads Docker's stats as `docker stats` does: memory is the
// working set (usage less inactive file pages: cgroup v2's inactive_file,
// v1's total_inactive_file); writes are the block I/O written, when the
// runtime reports it (gVisor doesn't).
func usageOf(st container.StatsResponse) isolation.Usage {
	u := isolation.Usage{CPUNanos: st.CPUStats.CPUUsage.TotalUsage, WrittenBytes: -1}
	mem := st.MemoryStats.Usage
	inactive := st.MemoryStats.Stats["inactive_file"]
	if v, ok := st.MemoryStats.Stats["total_inactive_file"]; ok {
		inactive = v
	}
	if inactive < mem {
		mem -= inactive
	}
	u.MemoryBytes = int64(mem) //nolint:gosec // bounded by the container's memory limit
	if es := st.BlkioStats.IoServiceBytesRecursive; es != nil {
		var w uint64
		for _, e := range es {
			if strings.EqualFold(e.Op, "write") {
				w += e.Value
			}
		}
		u.WrittenBytes = int64(w) //nolint:gosec // bytes written by one container
	}
	return u
}

func (m *mobyClient) ContainerInspect(ctx context.Context, id string) (ContainerState, error) {
	res, err := m.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerState{}, err
	}
	var st ContainerState
	if s := res.Container.State; s != nil {
		st.Running, st.ExitCode, st.OOMKilled = s.Running, s.ExitCode, s.OOMKilled
		st.StartedAt, _ = time.Parse(time.RFC3339Nano, s.StartedAt)
		st.FinishedAt, _ = time.Parse(time.RFC3339Nano, s.FinishedAt)
	}
	if res.Container.Config != nil {
		st.Labels = res.Container.Config.Labels
	}
	return st, nil
}

func (m *mobyClient) ContainerRemove(ctx context.Context, id string) error {
	_, err := m.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

func (m *mobyClient) ContainerList(ctx context.Context, label string) ([]ContainerSummary, error) {
	res, err := m.c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", label)})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, ContainerSummary{ID: c.ID, Labels: c.Labels, Running: c.State == container.StateRunning})
	}
	return out, nil
}

func (m *mobyClient) ContainerAttach(ctx context.Context, id string) (Attached, error) {
	res, err := m.c.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return Attached{}, err
	}
	return Attached{Conn: res.Conn, Reader: res.Reader}, nil
}

func (m *mobyClient) NetworkCreate(ctx context.Context, name string, labels map[string]string) (string, error) {
	res, err := m.c.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: labels})
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

func (m *mobyClient) ContainerIP(ctx context.Context, container, network string) (string, error) {
	res, err := m.c.ContainerInspect(ctx, container, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if ns := res.Container.NetworkSettings; ns != nil {
		if ep := ns.Networks[network]; ep != nil && ep.IPAddress.IsValid() {
			return ep.IPAddress.String(), nil
		}
	}
	return "", nil
}

func (m *mobyClient) NetworkConnect(ctx context.Context, network, container string) error {
	_, err := m.c.NetworkConnect(ctx, network, client.NetworkConnectOptions{Container: container})
	return err
}

func (m *mobyClient) NetworkDisconnect(ctx context.Context, network, container string) error {
	_, err := m.c.NetworkDisconnect(ctx, network, client.NetworkDisconnectOptions{Container: container, Force: true})
	return err
}

func (m *mobyClient) NetworkRemove(ctx context.Context, network string) error {
	_, err := m.c.NetworkRemove(ctx, network, client.NetworkRemoveOptions{})
	return err
}

func (m *mobyClient) NetworkList(ctx context.Context, label string) ([]NetworkSummary, error) {
	res, err := m.c.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("label", label)})
	if err != nil {
		return nil, err
	}
	out := make([]NetworkSummary, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, NetworkSummary{ID: n.ID, Name: n.Name, Labels: n.Labels})
	}
	return out, nil
}

// IsNotFound reports whether err means the container or image doesn't exist.
func IsNotFound(err error) bool {
	return err != nil && (cerrdefs.IsNotFound(err) || strings.Contains(err.Error(), "No such container") ||
		strings.Contains(err.Error(), "not found"))
}
