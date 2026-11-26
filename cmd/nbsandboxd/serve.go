// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/azbyluthfan/nbsandbox/internal/api"
	"github.com/azbyluthfan/nbsandbox/internal/audit"
	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/blob"
	"github.com/azbyluthfan/nbsandbox/internal/blob/azstore"
	"github.com/azbyluthfan/nbsandbox/internal/blob/s3store"
	"github.com/azbyluthfan/nbsandbox/internal/completions"
	"github.com/azbyluthfan/nbsandbox/internal/compute"
	"github.com/azbyluthfan/nbsandbox/internal/config"
	"github.com/azbyluthfan/nbsandbox/internal/deps"
	"github.com/azbyluthfan/nbsandbox/internal/grpcapi"
	"github.com/azbyluthfan/nbsandbox/internal/isolation"
	"github.com/azbyluthfan/nbsandbox/internal/obs"
	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/runs"
	"github.com/azbyluthfan/nbsandbox/internal/sessions"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
	"github.com/azbyluthfan/nbsandbox/internal/ui"
	"github.com/azbyluthfan/nbsandbox/internal/uiauth"
)

// minFreeDisk is the free space below which the data directory is not ready.
const minFreeDisk = 512 << 20

// serveOptions lets tests observe and stop the service.
type serveOptions struct {
	getenv     func(string) string
	logOut     io.Writer
	onListen   func(addr string) // called once the API listener is open
	onUIListen func(addr string) // called once the web UI listener is open
	storage    pingStore         // tests: instead of the configured object storage
}

func serve(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return run(ctx, args, serveOptions{getenv: os.Getenv, logOut: os.Stderr})
}

// run starts the service and blocks until ctx is cancelled (a signal in
// production), then shuts down gracefully.
func run(ctx context.Context, args []string, opt serveOptions) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", opt.getenv("NBSANDBOX_CONFIG"), "settings.yaml path (env NBSANDBOX_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 1. Configuration: invalid settings stop the service before anything starts.
	cfg, err := config.Load(*configPath, opt.getenv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	logger, err := obs.NewLogger(opt.logOut, cfg.LogLevel)
	if err != nil {
		return err
	}
	logger.Info("nbsandboxd starting", "version", version, "listen", cfg.Listen, "data_dir", cfg.DataDir,
		"docker_host", cfg.Docker.Host, "runtime", runtimeName(cfg.Docker.Runtime), "image", cfg.Docker.Image)

	// 2. Data directory and database.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if err := checkWritable(cfg.DataDir); err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	blobs := opt.storage
	if blobs == nil {
		if blobs, err = openStorage(ctx, cfg.Storage); err != nil {
			return err
		}
	}
	logger.Info("object storage ready", "kind", cfg.Storage.Kind)
	owners := map[string]spaces.OwnerLimits{}
	for _, o := range cfg.Quotas.Owners {
		owners[o.Owner] = spaces.OwnerLimits{MaxSpaces: o.MaxSpaces, MaxSpaceBytes: int64(o.MaxSpaceBytes)}
	}
	spaceSvc, err := spaces.New(spaces.Config{
		DataDir: cfg.DataDir, MaxFile: int64(cfg.Uploads.MaxFile), MaxSpace: int64(cfg.Uploads.MaxSpace),
		MaxSpaces: cfg.Quotas.MaxSpaces, Owners: owners,
	}, db, blobs, logger)
	if err != nil {
		return err
	}

	// 3. Authentication: users.yaml (basic auth and the web UI) and/or
	// bearer tokens from an OpenID Connect provider.
	authn := &auth.Authenticator{Log: logger}
	var users *auth.Users
	if cfg.UsersFile != "" && (cfg.Auth.BasicEnabled() || cfg.UI.Enabled) {
		if users, err = auth.LoadUsers(cfg.UsersFile); err != nil {
			return err
		}
		logger.Info("users loaded", "count", users.Count(), "basic_auth", cfg.Auth.BasicEnabled())
		if demo := users.DemoUsers(); len(demo) > 0 {
			logger.Warn("the users file has the repository's demo users, whose passwords are public: for local development only, never deploy them",
				"users", demo, "file", cfg.UsersFile)
		}
	}
	// One login limiter for every way to send a users.yaml password (basic
	// auth on the API, the web UI's sign-in form).
	limiter := auth.NewLoginLimiter(cfg.Auth.LoginLimit.Allowance())
	if cfg.Auth.BasicEnabled() {
		authn.Users = users
		authn.Limiter = limiter
		authn.LimitByAddress = cfg.Auth.ByAddress()
	}
	if authn.TrustedProxies, err = auth.NewTrustedProxies(cfg.Auth.TrustedProxies); err != nil {
		return err // validated at load; kept for safety
	}
	if cfg.Auth.OIDC != nil {
		if authn.OIDC, err = auth.NewVerifier(*cfg.Auth.OIDC, nil); err != nil {
			return err
		}
		logger.Info("bearer tokens accepted", "issuer", cfg.Auth.OIDC.Issuer, "audience", cfg.Auth.OIDC.Audience,
			"introspection", cfg.Auth.OIDC.Introspection != nil)
	}

	// 4. The backend (Docker or Kubernetes), on processes that run
	// notebooks. Unreachable is fine; a missing configured runtime is not.
	metrics := obs.NewMetrics()
	id := workerID(cfg)
	logger.Info("role", "role", cfg.Role, "worker_id", id, "backend", cfg.Backend)
	var (
		sup      backendSupervisor
		backend  isolation.Backend
		onChange = func(bool) {} // set once the run manager exists
	)
	dockerChanged := make(chan struct{}, 1)
	upChanged := func(up bool) {
		onChange(up)
		select { // tell the API nodes now
		case dockerChanged <- struct{}{}:
		default:
		}
	}
	if cfg.Executes() {
		var closeBackend func()
		var err error
		backend, sup, closeBackend, err = newBackend(cfg, id, logger, metrics, upChanged)
		if err != nil {
			return err
		}
		defer closeBackend()
	}

	// 5. Runs. The supervisor tells the manager when Docker comes and goes:
	// the queue pauses while it's down, and leftovers are reconciled when it
	// returns. An API node learns about the workers from the database.
	limits := isolation.Limits{
		MemoryBytes: int64(cfg.Limits.Memory), NanoCPUs: int64(cfg.Limits.CPUs * 1e9), PIDs: cfg.Limits.PIDs,
		TmpfsBytes: int64(cfg.Limits.TmpSize), ShmBytes: int64(cfg.Limits.ShmSize), LogBytes: int64(cfg.Limits.LogCap),
		StopGrace: cfg.Runs.StopGrace,
	}
	runDeps := runs.Deps{DB: db, Spaces: spaceSvc, Backend: backend, Metrics: metrics, Log: logger}
	conns, err := newConnections(cfg, db, logger)
	if err != nil {
		return fmt.Errorf("connections: %w", err)
	}
	if conns != nil {
		runDeps.Data = conns
	}
	comps := completions.NewPublisher(context.WithoutCancel(ctx), blobs, logger)
	runtime := func() (api.RuntimeInfo, error) { //nolint:contextcheck // the index is stored under the publisher's own (process) context
		return runtimeInfo(cfg, sup.Status(), comps)
	}
	if sup != nil {
		runDeps.Manifest = func() *deps.Manifest { return sup.Status().Manifest }
		runDeps.ImageID = func() string { return sup.Status().Image.ID }
	} else {
		workers := &workerView{db: db, within: cfg.Runs.Lease}
		runDeps.Manifest, runDeps.Available, runtime = workers.Manifest, workers.Available, workers.Runtime
		runDeps.ImageID = workers.ImageID
	}
	// Package sets: every node computes their keys; workers on Docker build
	// them.
	var builder isolation.PackageBuilder
	if pb, ok := backend.(isolation.PackageBuilder); ok && cfg.Executes() {
		builder = pb
	}
	pkgs := packages.New(packages.Config{
		Enabled: cfg.Packages.Enabled, IndexURL: cfg.Packages.IndexURL, Allowed: cfg.Packages.Allowed,
		MaxSize: int64(cfg.Packages.MaxSize), BuildTimeout: cfg.Packages.BuildTimeout, Retention: cfg.Packages.Retention,
		CacheSize: int64(cfg.Packages.CacheSize), BuilderImage: cfg.Packages.BuilderImage, Network: cfg.Packages.Network,
		Runtime: cfg.Docker.Runtime, Limits: limits, DataDir: cfg.DataDir, WorkerID: id, Platform: "linux/" + goruntime.GOARCH,
	}, db, blobs, builder, logger)
	runDeps.Packages = pkgs
	if cfg.Packages.Enabled {
		logger.Info("custom packages per space", "index", cfg.Packages.IndexURL, "allowed", len(cfg.Packages.Allowed),
			"builds_here", builder != nil, "builder_image", cfg.Packages.BuilderImage, "network", cfg.Packages.Network)
	}
	mgr := runs.New(runs.Config{
		Image: cfg.Docker.Image, Runtime: runtimeLabel(cfg), Limits: limits, Timeout: cfg.Limits.Timeout,
		Concurrency: cfg.Runs.Concurrency, LogCap: int64(cfg.Limits.LogCap), Retention: cfg.Runs.Retention,
		DependencyCheck: cfg.Runs.DependencyCheck, MaxParameters: int64(cfg.Runs.MaxParameters),
		MaxOutput: int64(cfg.Runs.MaxOutput), EventRetention: cfg.Runs.EventRetention,
		IdempotencyWindow: cfg.Runs.IdempotencyWindow, RetentionEvery: cfg.Runs.CleanupEvery,
		WorkerID: id, NoExecute: !cfg.Executes(), SharedWorkers: cfg.Role != config.RoleAll, Lease: cfg.Runs.Lease,
		RequireSchemas: cfg.Functions.RequireSchemas,
	}, runDeps)
	// Interactive sessions: held by workers whose backend runs them.
	var sessBackend isolation.SessionBackend
	if sb, ok := backend.(isolation.SessionBackend); ok && cfg.Executes() {
		sessBackend = sb
	}
	if cfg.Sessions.Enabled && !cfg.SessionsOn() {
		logger.Warn("interactive sessions are off: with API and worker processes, set sessions.secret (NBSANDBOX_SESSIONS_SECRET), the same on every process")
	}
	sess := sessions.New(sessions.Config{
		Enabled: cfg.SessionsOn(), Image: cfg.Docker.Image, Runtime: runtimeLabel(cfg), Limits: limits, Secret: cfg.Sessions.Secret,
		Idle: cfg.Sessions.Idle, Max: cfg.Sessions.Max, WriteBack: cfg.Sessions.WriteBack, Retention: cfg.Sessions.Retention,
		PerUser: cfg.Sessions.PerUser, PerWorker: cfg.Sessions.PerWorker, MaxMessage: int64(cfg.Sessions.MaxMessage),
		WorkerID: id, NoExecute: !cfg.Executes(), Shared: cfg.Role != config.RoleAll,
	}, sessions.Deps{
		DB: db, Spaces: spaceSvc, Backend: sessBackend, Log: logger, Data: sessionData(conns),
		Packages: sessionPackages(pkgs, spaceSvc, runDeps.ImageID),
		Draining: func() bool {
			d, err := db.GetDrain(context.WithoutCancel(ctx))
			return err == nil && d.Draining
		},
		Workers: func(ctx context.Context) ([]store.Worker, error) { return db.LiveWorkers(ctx, 3*cfg.Runs.Lease) },
	})
	if err := spaceSvc.CheckConsistency(ctx); err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	go pkgs.Run(ctx) // builds only on a worker with package sets on
	if err := sess.Start(ctx); err != nil {
		return err
	}
	if sess.Holds() {
		go db.Listen(context.WithoutCancel(ctx), logger, func(string, string) { sess.Wake() }, store.ChanSessions)
		logger.Info("interactive sessions", "idle", cfg.Sessions.Idle, "max", cfg.Sessions.Max,
			"per_user", cfg.Sessions.PerUser, "per_worker", cfg.Sessions.PerWorker)
	}
	// The background work outlives the signal: it stops only after the
	// graceful shutdown below, so it doesn't inherit ctx's cancellation.
	bg := context.WithoutCancel(ctx)
	supCtx, stopSup := context.WithCancel(bg)
	defer stopSup()
	checks := []obs.Checker{
		obs.CheckFunc{CheckName: "db", Fn: db.Ping},
		obs.CheckFunc{CheckName: "storage", Fn: blobs.Ping},
		obs.CheckFunc{CheckName: "data_dir", Fn: func(context.Context) error { return checkDataDir(cfg.DataDir) }},
	}
	var published chan struct{}
	if sup != nil {
		onChange = mgr.SetDockerUp
		if err := sup.CheckStartup(ctx); err != nil {
			return err
		}
		go sup.Run(supCtx)
		checks = append(checks,
			obs.CheckFunc{CheckName: "docker", Fn: sup.DockerCheck},
			obs.CheckFunc{CheckName: "runner_image", Fn: sup.ImageCheck},
			obs.CheckFunc{CheckName: "runtime", Fn: sup.RuntimeCheck})
		if cfg.Executes() { // a single process too: the compute view lists it
			published = make(chan struct{})
			go func() {
				defer close(published)
				publishWorker(supCtx, db, id, sessionAddr(cfg, sess, id), cfg, sup, comps, dockerChanged, logger)
			}()
		}
	}

	// 6. HTTP. Closing shuttingDown ends open log streams with a final event,
	// so they don't hold up the HTTP shutdown.
	shuttingDown := make(chan struct{})
	health := obs.NewHealth(3*time.Second, checks...)
	recorder := audit.New(audit.Config{Enabled: cfg.Audit.Enabled, Retention: cfg.Audit.Retention, Downloads: cfg.Audit.Downloads},
		db, logger, metrics.AuditFailures)
	go recorder.Run(ctx, cfg.Runs.CleanupEvery)
	deps := api.Deps{
		Connections: conns,
		Auth:        authn, Users: users, Health: health, Metrics: metrics, Logger: logger, Spaces: spaceSvc, Audit: recorder,
		MaxUpload:  int64(cfg.Uploads.MaxSpace) + 1<<20, // quota plus multipart overhead
		MaxRunBody: int64(cfg.Runs.MaxParameters) + 64<<10,
		Runs:       mgr,
		Sessions:   sess,
		Shutdown:   shuttingDown,
		Runtime:    runtime,
		Workers: func(ctx context.Context) ([]compute.Worker, error) {
			return compute.Workers(ctx, db, 3*cfg.Runs.Lease, time.Now())
		},
		Completions: func(ctx context.Context, ref completions.Ref) (io.ReadCloser, error) {
			return completions.Open(ctx, blobs, ref)
		},
	}
	var closeOnce sync.Once
	newServer := func(h http.Handler) *http.Server {
		s := &http.Server{
			Handler: h,
			// No ReadTimeout/WriteTimeout: uploads and SSE streams are
			// long-lived; handlers set their own deadlines.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			// Requests must not be cancelled by the shutdown signal itself;
			// Shutdown lets them finish.
			BaseContext: func(net.Listener) context.Context { return bg },
			ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		// gRPC needs HTTP/2; without TLS here (the proxy in front terminates it).
		s.Protocols = new(http.Protocols)
		s.Protocols.SetHTTP1(true)
		s.Protocols.SetHTTP2(true)
		s.Protocols.SetUnencryptedHTTP2(true)
		s.RegisterOnShutdown(func() { closeOnce.Do(func() { close(shuttingDown) }) })
		return s
	}
	serveErr := make(chan error, 2)
	listen := func(addr string, s *http.Server, name string, hook func(string)) error {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		go func() { serveErr <- s.Serve(ln) }()
		logger.Info(name+" listening", "addr", ln.Addr().String())
		if hook != nil {
			hook(ln.Addr().String())
		}
		return nil
	}
	handler := withGRPC(api.NewHandler(deps), grpcapi.Handler(grpcapi.Deps{
		Auth: authn, Spaces: spaceSvc, Runs: mgr, Runtime: runtime, Logger: logger, Shutdown: shuttingDown,
		Completions: deps.Completions, Audit: recorder, Sessions: sess, Connections: conns, Workers: deps.Workers,
	}))
	if !cfg.Serves() {
		handler = workerHandler(health, metrics)
	}
	srv := newServer(handler)
	if err := listen(cfg.Listen, srv, "nbsandboxd", opt.onListen); err != nil {
		return err
	}
	// The optional web UI on its own listener, with or without sign-in.
	var uiSrv *http.Server
	if cfg.UI.Enabled && cfg.Serves() {
		sessions, err := uiauth.New(cfg.UI.SignIn(), uiauth.Deps{
			Store: db, Verifier: authn.OIDC, Log: logger, Audit: recorder,
			Basic: &auth.Authenticator{Users: users, Limiter: limiter, LimitByAddress: cfg.Auth.ByAddress(),
				TrustedProxies: authn.TrustedProxies, Log: logger},
		})
		if err != nil {
			return err
		}
		if sessions == nil {
			if !config.IsLoopback(cfg.UI.Listen) {
				logger.Warn("the web UI has no sign-in (ui.auth: none) and listens beyond loopback: anyone who reaches it acts as any user",
					"addr", cfg.UI.Listen)
			}
		} else {
			go sessions.Run(ctx, 10*time.Minute)
			logger.Info("web UI sign-in", "auth", cfg.UI.Auth, "public_url", cfg.UI.PublicURL)
		}
		uiSrv = newServer(api.NewUIHandler(deps, ui.Static(), sessions))
		if err := listen(cfg.UI.Listen, uiSrv, "web UI", opt.onUIListen); err != nil {
			return err
		}
	}

	// A worker of a cluster serves its sessions to the API nodes on an
	// internal listener (authenticated with the shared secret).
	var sessSrv *http.Server
	if cfg.Role == config.RoleWorker && sess.Holds() {
		sessSrv = newServer(sess.InternalHandler())
		if err := listen(cfg.Sessions.Listen, sessSrv, "sessions (internal)", nil); err != nil {
			return err
		}
	}

	// 7. Run until a signal, then shut down gracefully.
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down", "grace", cfg.ShutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(bg, 15*time.Second)
	defer cancel()
	// Stop accepting requests, then stop dequeuing, wait up to the grace
	// period for active runs, and stop the rest as interrupted. Open log
	// streams get a final event.
	for _, s := range []*http.Server{srv, uiSrv, sessSrv} {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("http shutdown", "error", err)
		}
	}
	mgr.Shutdown(cfg.ShutdownGrace)
	sess.Shutdown(shutdownCtx) // sessions keep running; the next start re-attaches
	stopSup()
	if published != nil {
		<-published
	}
	logger.Info("nbsandboxd stopped")
	return nil
}

// runtimeInfo is GET /v1/runtime: the image nbsandboxd actually runs, read from
// Docker, and the limits in effect.
func runtimeInfo(cfg config.Settings, st isolation.Status, comps *completions.Publisher) (api.RuntimeInfo, error) {
	if !st.Up || st.Manifest == nil {
		return api.RuntimeInfo{}, runs.ErrDockerUnavailable
	}
	var info api.RuntimeInfo
	info.Image.Ref, info.Image.ID, info.Image.RepoDigests = cfg.Docker.Image, st.Image.ID, st.Image.RepoDigests
	if info.Image.RepoDigests == nil {
		info.Image.RepoDigests = []string{}
	}
	info.PythonVersion = st.Manifest.PythonVersion
	info.Packages = st.Manifest.Distributions
	info.Modules = st.Manifest.Modules()
	info.Runtime = runtimeLabel(cfg)
	info.Docker.Version, info.Docker.OperatingSystem = st.Info.ServerVersion, st.Info.OperatingSystem
	info.Limits = api.RuntimeLimits{
		MemoryBytes: int64(cfg.Limits.Memory), CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs,
		TmpBytes: int64(cfg.Limits.TmpSize), TimeoutSeconds: cfg.Limits.Timeout.Seconds(), LogBytes: int64(cfg.Limits.LogCap),
		MaxFileBytes: int64(cfg.Uploads.MaxFile), MaxSpaceBytes: int64(cfg.Uploads.MaxSpace), Concurrency: cfg.Runs.Concurrency,
	}
	if comps != nil {
		info.Completions = comps.Ref(st.Completions)
	}
	return info, nil
}

func runtimeName(r string) string {
	if r == "" {
		return "runc (default)"
	}
	return r
}

// checkWritable fails startup if the data directory cannot be written.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("data dir %s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// checkDataDir is the "data_dir" readiness check: present, a directory, and
// with enough free space.
func checkDataDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return err
	}
	free := st.Bavail * uint64(st.Bsize) //nolint:gosec,unconvert // Bsize type differs per OS; never negative
	if free < minFreeDisk {
		return fmt.Errorf("only %d MiB free in %s (need %d MiB)", free>>20, dir, minFreeDisk>>20)
	}
	return nil
}

// pingStore is a blob store that can report whether it is reachable.
type pingStore interface {
	blob.Store
	Ping(context.Context) error
}

// openStorage connects to the configured object storage. Its settings may
// hold secrets (keys, connection strings), so errors never include them.
func openStorage(ctx context.Context, st config.Storage) (pingStore, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch st.Kind {
	case "s3":
		return s3store.New(ctx, st.S3)
	case "azure":
		return azstore.New(ctx, st.Azure)
	}
	return nil, fmt.Errorf("storage.kind %q is not s3 or azure", st.Kind)
}

// runtimeLabel names what isolates runs: the Docker runtime, or the
// Kubernetes RuntimeClass ("runc" when neither is set).
func runtimeLabel(cfg config.Settings) string {
	switch {
	case cfg.Backend == "kubernetes" && cfg.Kubernetes.RuntimeClass != "":
		return cfg.Kubernetes.RuntimeClass
	case cfg.Backend != "kubernetes" && cfg.Docker.Runtime != "":
		return cfg.Docker.Runtime
	}
	return "runc"
}
