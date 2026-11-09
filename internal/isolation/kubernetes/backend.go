// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// cleanupTimeout bounds each API call made after a run's context ended.
const cleanupTimeout = 30 * time.Second

// Backend runs notebooks as Pods.
type Backend struct {
	cfg    Config
	client kubernetes.Interface
	rest   *rest.Config
	log    *slog.Logger
	// MaxCopyOut bounds what is copied back from a Pod (the run's folder and,
	// for a writing run, the space).
	MaxCopyOut int64
	// poll is how often Pod state is read while waiting.
	poll time.Duration
}

// NewBackend creates the backend. restCfg is used for exec (copying files).
func NewBackend(cfg Config, client kubernetes.Interface, restCfg *rest.Config, log *slog.Logger) *Backend {
	if cfg.SpaceBytes == 0 {
		cfg.SpaceBytes = 1 << 30
	}
	return &Backend{cfg: cfg, client: client, rest: restCfg, log: log, MaxCopyOut: 2 * cfg.SpaceBytes, poll: 500 * time.Millisecond}
}

// Name implements isolation.Backend.
func (b *Backend) Name() string { return "kubernetes" }

func freshContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// Run implements isolation.Backend. The runner waits while the worker
// copies /space and /run in through the files container; then it runs while
// its output is followed; then /run (and /space for a writing run) is copied
// back out through the files container, and the Pod is deleted.
func (b *Backend) Run(ctx context.Context, spec isolation.RunSpec, out isolation.LineSink) (isolation.RunResult, error) {
	var res isolation.RunResult
	pods := b.client.CoreV1().Pods(b.cfg.Namespace)
	name := PodName(spec.RunID)
	cctx, cancel := freshContext(ctx)
	pod, err := pods.Create(cctx, buildPod(b.cfg, spec), metav1.CreateOptions{})
	cancel()
	if err != nil {
		return res, fmt.Errorf("%w: create pod: %w", isolation.ErrStart, err)
	}
	res.ContainerID = string(pod.UID)
	remove := func() {
		cctx, cancel := freshContext(ctx)
		defer cancel()
		zero := int64(0)
		if err := pods.Delete(cctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
			b.log.Warn("delete run pod", "run_id", spec.RunID, "error", err)
			return
		}
		res.Removed = true
	}

	// Copy the files in once the files container runs (the runner waits).
	if err := b.waitFor(ctx, name, func(p *corev1.Pod) (bool, error) { return running(p, filesContainer, false), podFailed(p) }); err != nil {
		remove()
		if ctx.Err() != nil {
			res.Stopped = true
			return res, nil
		}
		return res, fmt.Errorf("%w: the pod didn't start: %w", isolation.ErrStart, err)
	}
	if err := b.copyIn(ctx, name, spec); err != nil {
		remove()
		return res, fmt.Errorf("%w: copy the run's files in: %w", isolation.ErrStart, err)
	}
	// Events (items, progress, cells) come back while the run works. The
	// follower starts now, not once the runner has: opening an exec can take
	// seconds on a busy cluster, and started late it would deliver the first
	// items together instead of as they are written.
	eventsCtx, stopEvents := context.WithCancel(context.WithoutCancel(ctx))
	defer stopEvents()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		b.syncEvents(eventsCtx, name, spec.RunDir)
	}()

	// Follow the runner's output.
	if err := b.waitFor(ctx, name, func(p *corev1.Pod) (bool, error) {
		return started(p, runnerContainer), podFailed(p)
	}); err != nil {
		remove()
		if ctx.Err() != nil {
			res.Stopped = true
			return res, nil
		}
		return res, fmt.Errorf("%w: the runner didn't start: %w", isolation.ErrStart, err)
	}
	logsCtx, stopLogs := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLogs()
	followDone := make(chan struct{})
	go func() {
		defer close(followDone)
		b.follow(logsCtx, name, out)
	}()
	// The run's files are in the Pod: watch their size there.
	var overDisk atomic.Bool
	watchCtx, stopWatch := context.WithCancel(logsCtx)
	defer stopWatch()
	if spec.DiskBytes > 0 {
		go b.watchDisk(watchCtx, name, spec.DiskBytes, func() {
			overDisk.Store(true)
			b.terminate(ctx, name)
		})
	}
	// Wait for the runner to end; on cancel, stop it (TERM, then KILL).
	ended := func(p *corev1.Pod) (bool, error) { return terminated(p, runnerContainer) != nil, nil }
	waitCtx, stopWait := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWait()
	waitErr := make(chan error, 1)
	go func() { waitErr <- b.waitFor(waitCtx, name, ended) }()
	var lost error
	select {
	case lost = <-waitErr:
	case <-ctx.Done():
		res.Stopped = true
		b.terminate(ctx, name)
		select {
		case lost = <-waitErr:
		case <-time.After(spec.Limits.StopGrace):
			// It ignored SIGTERM. Inside its own PID namespace even SIGKILL
			// can't reach the runner's PID 1, so the Pod goes, outputs and
			// all.
			b.log.Warn("the runner ignored SIGTERM; deleting its pod", "run_id", spec.RunID)
			stopLogs()
			<-followDone
			remove()
			return res, nil
		}
	}
	stopWatch()
	stopEvents()
	<-syncDone // before the run's folder is copied back over its events
	select {
	case <-followDone:
	case <-time.After(10 * time.Second):
		stopLogs()
		<-followDone
	}
	if overDisk.Load() {
		// Nothing is copied back: the run's changes would exceed the quota.
		out.Line("[nbsandbox] the run was stopped: the run's changes to the space were not stored: they would exceed the space's quota")
		res.DiskExceeded = true
		remove()
		return res, nil
	}

	cctx, cancel = freshContext(ctx)
	pod, err = pods.Get(cctx, name, metav1.GetOptions{})
	cancel()
	if err != nil {
		return res, fmt.Errorf("%w: %w", isolation.ErrDockerLost, errors.Join(lost, err))
	}
	if evicted(pod) {
		// The kubelet evicts a Pod over its ephemeral-storage limit.
		res.DiskExceeded = true
		remove()
		return res, nil
	}
	st := terminated(pod, runnerContainer)
	if st == nil {
		return res, fmt.Errorf("%w: %w", isolation.ErrDockerLost, errors.Join(lost, errors.New("the runner's state is unknown")))
	}
	res.ExitCode = int(st.ExitCode)
	res.OOMKilled = oomKilled(st, res.Stopped)
	res.Started, res.Finished = st.StartedAt.Time, st.FinishedAt.Time
	if err := b.copyOut(ctx, name, spec); err != nil {
		if errors.Is(err, ErrTooLarge) {
			res.DiskExceeded = true
		} else {
			b.log.Warn("copy the run's files out", "run_id", spec.RunID, "error", err)
		}
	}
	remove()
	return res, nil
}

// oomKilled reports whether the kernel's OOM killer ended the runner. With
// cgroup v2 it kills the whole container, wrapper included. Most runtimes
// say so (reason OOMKilled), but some (containerd nested in Docker, as in
// kind) only report SIGKILL. Nothing else sends SIGKILL to a runner the
// worker didn't stop and the kubelet didn't evict, so that counts too.
func oomKilled(st *corev1.ContainerStateTerminated, stopped bool) bool {
	return st.Reason == "OOMKilled" || (st.ExitCode == 137 && !stopped)
}

// waitFor polls the Pod until done reports true, the Pod fails, or ctx ends.
func (b *Backend) waitFor(ctx context.Context, name string, done func(*corev1.Pod) (bool, error)) error {
	t := time.NewTicker(b.poll)
	defer t.Stop()
	misses := 0
	for {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		p, err := b.client.CoreV1().Pods(b.cfg.Namespace).Get(cctx, name, metav1.GetOptions{})
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			if misses++; misses > 60 { // about 30 s without the API
				return err
			}
		case err == nil:
			misses = 0
			ok, failure := done(p)
			if ok {
				return nil
			}
			if failure != nil {
				return failure
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func status(p *corev1.Pod, container string, init bool) *corev1.ContainerStatus {
	list := p.Status.ContainerStatuses
	if init {
		list = p.Status.InitContainerStatuses
	}
	for i := range list {
		if list[i].Name == container {
			return &list[i]
		}
	}
	return nil
}

func running(p *corev1.Pod, container string, init bool) bool {
	s := status(p, container, init)
	return s != nil && s.State.Running != nil
}

func started(p *corev1.Pod, container string) bool {
	s := status(p, container, false)
	return s != nil && (s.State.Running != nil || s.State.Terminated != nil)
}

func terminated(p *corev1.Pod, container string) *corev1.ContainerStateTerminated {
	if s := status(p, container, false); s != nil {
		return s.State.Terminated
	}
	return nil
}

func evicted(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodFailed && p.Status.Reason == "Evicted"
}

// podFailed explains a Pod that can't run: image missing, unschedulable for
// too long, evicted.
func podFailed(p *corev1.Pod) error {
	if p.Status.Phase == corev1.PodFailed {
		return fmt.Errorf("pod failed: %s %s", p.Status.Reason, p.Status.Message)
	}
	for _, s := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := s.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" ||
			w.Reason == "InvalidImageName" || w.Reason == "CreateContainerConfigError" || w.Reason == "CreateContainerError") {
			return fmt.Errorf("%s: %s", w.Reason, w.Message)
		}
	}
	return nil
}

// follow streams the runner's output into out, one line at a time.
func (b *Backend) follow(ctx context.Context, name string, out isolation.LineSink) {
	req := b.client.CoreV1().Pods(b.cfg.Namespace).GetLogs(name, &corev1.PodLogOptions{Container: runnerContainer, Follow: true})
	stream, err := req.Stream(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("follow run output", "pod", name, "error", err)
		}
		return
	}
	defer func() { _ = stream.Close() }()
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out.Line(strings.TrimSuffix(sc.Text(), "\r"))
	}
}

// exec runs cmd in a container of the Pod, with stdin, and returns stdout.
func (b *Backend) exec(ctx context.Context, name, container string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	req := b.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(b.cfg.Namespace).Name(name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdin: stdin != nil, Stdout: stdout != nil, Stderr: true},
			scheme.ParameterCodec)
	// SPDY, which every Kubernetes version serves. (A WebSocket-first
	// fallback executor would consume stdin on the failed first attempt.)
	ex, err := remotecommand.NewSPDYExecutor(b.rest, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: &stderr})
	if err != nil && stderr.Len() > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return err
}

// copyIn puts the space's files and the run's folder into the Pod through
// the files container, then lets the runner start.
func (b *Backend) copyIn(ctx context.Context, name string, spec isolation.RunSpec) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	for _, d := range []struct{ local, remote string }{{spec.SpaceDir, "/space"}, {spec.RunDir, "/run"}} {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeTar(pw, d.local, workerOwned)) }()
		err := b.exec(cctx, name, filesContainer, []string{"tar", "-x", "-m", "--no-same-owner", "-C", d.remote, "-f", "-"}, pr, nil)
		_ = pr.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", d.remote, err)
		}
	}
	return b.exec(cctx, name, filesContainer, []string{"touch", stageDone}, nil, nil)
}

// copyOut brings /run (and /space for a writing run) back from the Pod
// through the files container. The local folders are replaced, so files the
// run deleted are gone here too.
func (b *Backend) copyOut(ctx context.Context, name string, spec isolation.RunSpec) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	type dir struct {
		local, remote string
		replace       bool // mirror deletions too (the space)
	}
	dirs := []dir{{spec.RunDir, "/run", false}}
	if !spec.SpaceReadOnly {
		dirs = append(dirs, dir{spec.SpaceDir, "/space", true})
	}
	// Measure first: a run that filled its allowance isn't copied back
	// (that could be gigabytes through the API) and ends as over its disk.
	var sizes bytes.Buffer
	remotes := []string{"du", "-s", "-k"}
	for _, d := range dirs {
		remotes = append(remotes, d.remote)
	}
	if err := b.exec(cctx, name, filesContainer, remotes, nil, &sizes); err == nil && duKiB(sizes.String())*1024 > b.MaxCopyOut {
		return ErrTooLarge
	}
	budget := b.MaxCopyOut
	for _, d := range dirs {
		if d.replace {
			if err := clearDir(d.local); err != nil {
				return err
			}
		}
		pr, pw := io.Pipe()
		errc := make(chan error, 1)
		var n int64
		go func() {
			_, err := readTar(&countReader{r: pr, n: &n}, d.local, budget)
			if err != nil {
				_ = pr.CloseWithError(err)
			} else {
				_, _ = io.Copy(io.Discard, pr) // tar's trailing padding
			}
			errc <- err
		}()
		cmd := []string{"tar", "-c", "-C", d.remote}
		if !d.replace {
			cmd = append(cmd, "--exclude=./"+workerOwned) // the worker's own copy is the full log
		}
		err := b.exec(cctx, name, filesContainer, append(cmd, "."), nil, pw)
		_ = pw.CloseWithError(err)
		if rerr := <-errc; rerr != nil {
			return fmt.Errorf("%s: %w", d.remote, rerr)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", d.remote, err)
		}
		budget -= n
	}
	return nil
}

type countReader struct {
	r io.Reader
	n *int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// terminate sends SIGTERM to the runner's main process (the wrapper, which
// stops the kernel and records the run's state).
func (b *Backend) terminate(ctx context.Context, name string) {
	cctx, cancel := freshContext(ctx)
	defer cancel()
	cmd := []string{"python", "-c", "import os, signal; os.kill(1, signal.SIGTERM)"}
	if err := b.exec(cctx, name, runnerContainer, cmd, nil, nil); err != nil {
		b.log.Debug("signal the runner", "pod", name, "error", err)
	}
}

// Orphans implements isolation.Backend: run Pods that exist.
func (b *Backend) Orphans(ctx context.Context) ([]isolation.Orphan, error) {
	list, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelRun})
	if err != nil {
		return nil, err
	}
	out := make([]isolation.Orphan, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, isolation.Orphan{RunID: p.Labels[LabelRun], SpaceID: p.Labels[LabelSpace], ContainerID: string(p.UID),
			Running: p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending})
	}
	return out, nil
}

// Kill implements isolation.Backend: delete the run's Pod, if any.
func (b *Backend) Kill(ctx context.Context, runID string) error {
	zero := int64(0)
	err := b.client.CoreV1().Pods(b.cfg.Namespace).Delete(ctx, PodName(runID), metav1.DeleteOptions{GracePeriodSeconds: &zero})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// workerOwned is the file in the run's folder that the worker writes (the
// log, from the runner's output): never copied in or out.
const workerOwned = "log.txt"

// duKiB adds up the sizes du -s -k printed.
func duKiB(out string) int64 {
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var n int64
		if _, err := fmt.Sscan(line, &n); err == nil {
			total += n
		}
	}
	return total
}

// diskPoll is how often a run's files are measured in its Pod (as the run
// manager does on the host for Docker).
const diskPoll = 5 * time.Second

// eventsRetry is how long to wait before following the events file again
// after the stream broke (the API server restarted, say).
const eventsRetry = time.Second

// followEvents prints what is appended to the events file from byte $1 on,
// checking its size four times a second. Not `tail -f`: it waits for inotify,
// which doesn't fire for another container's writes in a gVisor sandbox, so
// events arrived only when the run ended. Only a regular file is read, never
// a link the notebook planted.
const followEvents = `f=/run/` + isolation.RunEventsFile + `; off=$1
while :; do
  if [ -f "$f" ] && [ ! -L "$f" ]; then
    size=$(stat -c %s "$f")
    if [ "$size" -gt "$off" ]; then
      tail -c "+$((off + 1))" "$f" | head -c "$((size - off))"
      off=$size
    fi
  fi
  sleep 0.25
done`

// syncEvents follows the run's events file in the Pod and appends what the
// run writes to the worker's copy of the run's folder, where nbsandboxd reads
// it as on Docker (where the folder is shared): items and progress reach
// callers as they are written. One long-lived exec per run (an exec per poll
// was too slow on a busy cluster); if it breaks, it starts again where it
// stopped. The whole folder comes back when the run ends.
func (b *Backend) syncEvents(ctx context.Context, name, localDir string) {
	w := &appender{dir: localDir, file: isolation.RunEventsFile}
	for ctx.Err() == nil {
		cmd := []string{"sh", "-c", followEvents, "follow-events", strconv.FormatInt(w.n, 10)}
		if err := b.exec(ctx, name, filesContainer, cmd, nil, w); err != nil && ctx.Err() == nil {
			b.log.Debug("follow run events; starting again", "pod", name, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(eventsRetry):
		}
	}
}

// appender appends what it is given to dir/file and counts it.
type appender struct {
	dir, file string
	n         int64
}

func (a *appender) Write(p []byte) (int, error) {
	n, err := appendTo(a.dir, a.file, p)
	a.n += int64(n)
	return n, err
}

// appendTo appends data to dir/file, inside dir only.
func appendTo(dir, file string, data []byte) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(data)
	return n, errors.Join(err, f.Close())
}

// watchDisk measures /space and /run in the Pod and calls over once they
// hold more than limit bytes.
func (b *Backend) watchDisk(ctx context.Context, name string, limit int64, over func()) {
	t := time.NewTicker(diskPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var out bytes.Buffer
		if err := b.exec(ctx, name, filesContainer, []string{"du", "-s", "-k", "/space", "/run"}, nil, &out); err != nil {
			continue
		}
		if duKiB(out.String())*1024 > limit {
			over()
			return
		}
	}
}
