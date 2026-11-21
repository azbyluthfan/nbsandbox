// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/azbyluthfan/nbsandbox/internal/isolation"
)

// LabelSession marks interactive session Pods (not runs: the run
// reconciler never sees them).
const LabelSession = "nbsandbox.session"

// SessionPodName is a session's Pod name; one per session.
func SessionPodName(sessionID string) string { return "nbsandbox-session-" + sessionID }

// buildSessionPod is a run's Pod (every isolation control, pinned by the
// run's tests), with the session bridge as the runner container's process
// and its stdin kept open for attaches. Like a run, it waits for its files.
func buildSessionPod(cfg Config, spec isolation.SessionSpec) *corev1.Pod {
	pod := buildPod(cfg, isolation.RunSpec{
		RunID: spec.SessionID, SpaceID: spec.SpaceID, Image: spec.Image, Limits: spec.Limits, SpaceReadOnly: spec.SpaceReadOnly,
	})
	pod.Name = SessionPodName(spec.SessionID)
	pod.Labels = map[string]string{LabelSession: spec.SessionID, LabelSpace: spec.SpaceID, LabelWorker: labelValue(cfg.WorkerID),
		LabelRole: "session", LabelManaged: "nbsandbox"}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != runnerContainer {
			continue
		}
		c.Args = []string{"python", isolation.SessionBridge}
		env := []corev1.EnvVar{
			{Name: "HOME", Value: "/tmp"},
			{Name: "PYTHONUNBUFFERED", Value: "1"},
			{Name: "NBSANDBOX_SESSION_ID", Value: spec.SessionID},
			{Name: "NBSANDBOX_SESSION_MAX_MESSAGE", Value: strconv.FormatInt(spec.MaxMessage, 10)},
		}
		for _, e := range c.Env {
			if e.Name == "NBSANDBOX_NPROC" { // the gVisor process limit, as for runs
				env = append(env, e)
			}
		}
		c.Env = env
		c.Stdin, c.StdinOnce = true, false
	}
	return pod
}

// StartSession implements isolation.SessionBackend: the Pod, attached
// before its files go in (the bridge starts once they are), so nothing it
// says is missed.
func (b *Backend) StartSession(ctx context.Context, spec isolation.SessionSpec) (isolation.SessionConn, error) {
	pods := b.client.CoreV1().Pods(b.cfg.Namespace)
	name := SessionPodName(spec.SessionID)
	cctx, cancel := freshContext(ctx)
	_, err := pods.Create(cctx, buildSessionPod(b.cfg, spec), metav1.CreateOptions{})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w: create the session's pod: %w", isolation.ErrStart, err)
	}
	fail := func(err error) (isolation.SessionConn, error) {
		_ = b.StopSession(ctx, spec.SessionID)
		return nil, err
	}
	if err := b.waitFor(ctx, name, func(p *corev1.Pod) (bool, error) {
		return running(p, filesContainer, false) && running(p, runnerContainer, false), podFailed(p)
	}); err != nil {
		return fail(fmt.Errorf("%w: the session's pod didn't start: %w", isolation.ErrStart, err))
	}
	conn, err := b.attach(ctx, name)
	if err != nil {
		return fail(fmt.Errorf("%w: attach to the session: %w", isolation.ErrStart, err))
	}
	if err := b.copyIn(ctx, name, isolation.RunSpec{SpaceDir: spec.SpaceDir}); err != nil {
		_ = conn.Close()
		return fail(fmt.Errorf("%w: copy the space's files in: %w", isolation.ErrStart, err))
	}
	return conn, nil
}

// AttachSession implements isolation.SessionBackend.
func (b *Backend) AttachSession(ctx context.Context, sessionID string) (isolation.SessionConn, error) {
	return b.attach(ctx, SessionPodName(sessionID))
}

// StopSession implements isolation.SessionBackend: the Pod goes, at once
// (the kernel's state goes with it; files were pulled first).
func (b *Backend) StopSession(ctx context.Context, sessionID string) error {
	cctx, cancel := freshContext(ctx)
	defer cancel()
	zero := int64(0)
	err := b.client.CoreV1().Pods(b.cfg.Namespace).Delete(cctx, SessionPodName(sessionID), metav1.DeleteOptions{GracePeriodSeconds: &zero})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Sessions implements isolation.SessionBackend.
func (b *Backend) Sessions(ctx context.Context) ([]isolation.SessionInfo, error) {
	list, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelSession})
	if err != nil {
		return nil, err
	}
	out := make([]isolation.SessionInfo, 0, len(list.Items))
	for _, p := range list.Items {
		// Its kernel lives in the runner container: the Pod's phase stays
		// Running while the files container does.
		alive := p.Status.Phase == corev1.PodPending ||
			(p.Status.Phase == corev1.PodRunning && terminated(&p, runnerContainer) == nil)
		out = append(out, isolation.SessionInfo{SessionID: p.Labels[LabelSession], SpaceID: p.Labels[LabelSpace], Running: alive})
	}
	return out, nil
}

// PullSessionFiles implements isolation.SessionFilePuller: the session's
// /space, copied over localDir (its working copy on this worker), before
// a write-back.
func (b *Backend) PullSessionFiles(ctx context.Context, sessionID, localDir string) error {
	return b.copyOut(ctx, SessionPodName(sessionID), isolation.RunSpec{SpaceDir: localDir})
}

// attach connects to the runner container's stdin, stdout and stderr.
func (b *Backend) attach(ctx context.Context, name string) (isolation.SessionConn, error) {
	req := b.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(b.cfg.Namespace).Name(name).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{Container: runnerContainer, Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(b.rest, http.MethodPost, req.URL())
	if err != nil {
		return nil, err
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stdout := &gate{w: outW}
	stderr := &gate{w: &lineSink{log: func(line string) { b.log.Warn("session bridge", "pod", name, "stderr", line) }}}
	c := &attachConn{r: outR, w: inW, cancel: cancel}
	go func() {
		err := streamOrAbandon(actx, func(ctx context.Context) error {
			return ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: inR, Stdout: stdout, Stderr: stderr})
		}, func() {
			stdout.close()
			stderr.close()
		})
		if err == nil {
			err = io.EOF
		}
		_ = outW.CloseWithError(err)
		_ = inR.Close()
	}()
	return c, nil
}

// attachConn is a SessionConn over a Pod attach.
type attachConn struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	cancel context.CancelFunc
	mu     sync.Mutex
	once   sync.Once
}

func (c *attachConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *attachConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

func (c *attachConn) Close() error {
	c.once.Do(func() {
		c.cancel()
		_ = c.w.Close()
		_ = c.r.CloseWithError(errors.New("detached"))
	})
	return nil
}

// lineSink logs what it is given a line at a time, at most 50 lines.
type lineSink struct {
	log  func(string)
	buf  []byte
	n    int
	quit bool
}

func (l *lineSink) Write(p []byte) (int, error) {
	if l.quit {
		return len(p), nil
	}
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		line := string(l.buf[:i])
		l.buf = l.buf[i+1:]
		if l.n++; l.n > 50 {
			l.log("(more stderr not logged)")
			l.quit, l.buf = true, nil
			return len(p), nil
		}
		l.log(line)
	}
	if len(l.buf) > 64<<10 {
		l.log(string(l.buf))
		l.buf = nil
	}
	return len(p), nil
}
