// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/azbyluthfan/nbsandbox/internal/auth"
	"github.com/azbyluthfan/nbsandbox/internal/blob/azstore"
	"github.com/azbyluthfan/nbsandbox/internal/blob/s3store"
)

// Settings is the whole service configuration. Limits apply to every run and
// are never taken from requests.
type Settings struct {
	Listen  string `yaml:"listen"`
	DataDir string `yaml:"data_dir"`
	// Role is what this process does: "all" (default: one process does
	// everything), "api" (REST, gRPC and UI; never runs notebooks) or
	// "worker" (runs notebooks; serves only health and metrics). Several api
	// and worker processes share the database and object storage.
	Role string `yaml:"role"`
	// WorkerID names this worker in run leases (default: the host name).
	WorkerID string `yaml:"worker_id"`
	// DatabaseURL is the PostgreSQL database (postgres://user:pass@host:5432/db?sslmode=...).
	// It carries a password: prefer NBSANDBOX_DATABASE_URL, and it is never logged.
	DatabaseURL string `yaml:"database_url"`
	UsersFile   string `yaml:"users_file"`
	LogLevel    string `yaml:"log_level"`

	// Backend is what workers run notebooks with: "docker" (default) or
	// "kubernetes" (one Pod per run, in a cluster).
	Backend    string     `yaml:"backend"`
	Docker     Docker     `yaml:"docker"`
	Kubernetes Kubernetes `yaml:"kubernetes"`
	Limits     Limits     `yaml:"limits"`
	Uploads    Uploads    `yaml:"uploads"`
	Quotas     Quotas     `yaml:"quotas"`
	Runs       Runs       `yaml:"runs"`

	Functions Functions `yaml:"functions"`

	ShutdownGrace time.Duration `yaml:"shutdown_grace"`

	UI UI `yaml:"ui"`

	Auth Auth `yaml:"auth"`

	Storage Storage `yaml:"storage"`
}

// Storage is where file contents live: an S3-compatible store or Azure Blob
// Storage. The file index is in the database; only contents go here.
type Storage struct {
	// Kind is s3 or azure.
	Kind  string         `yaml:"kind"`
	S3    s3store.Config `yaml:"s3"`
	Azure azstore.Config `yaml:"azure"`
}

// Auth chooses how callers authenticate: basic auth against users_file, bearer
// tokens from an OpenID Connect provider, or both.
type Auth struct {
	// Basic enables basic auth against users_file (default true).
	Basic *bool `yaml:"basic"`
	// OIDC enables bearer tokens; absent = off.
	OIDC *auth.OIDCConfig `yaml:"oidc"`
}

// BasicEnabled reports whether basic auth is on (the default).
func (a Auth) BasicEnabled() bool { return a.Basic == nil || *a.Basic }

// UI is the optional local web UI. It acts as any user without
// a password, so it is off by default and listens on loopback by default.
type UI struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// Docker configures how nbsandboxd reaches Docker and what it runs.
type Docker struct {
	// Host is the Docker API address, e.g. unix:///var/run/docker.sock or
	// tcp://socket-proxy:2375. Empty means DOCKER_HOST, then the default socket.
	Host string `yaml:"host"`
	// Runtime is "" (Docker's default, runc) or "runsc" (gVisor). Never falls back.
	Runtime string `yaml:"runtime"`
	// Image is the runner image reference.
	Image string `yaml:"image"`
}

// Kubernetes configures the kubernetes backend: where run Pods go and how.
// The runner image is docker.image.
type Kubernetes struct {
	// Namespace holds the run Pods (it must exist, with a deny-all
	// NetworkPolicy for them; the Helm chart creates both).
	Namespace string `yaml:"namespace"`
	// RuntimeClass, if set, runs Pods with that RuntimeClass (e.g. kata or
	// gvisor); it must exist, and is never replaced by the default.
	RuntimeClass string            `yaml:"runtime_class"`
	NodeSelector map[string]string `yaml:"node_selector"`
	// Tolerations let run Pods onto tainted nodes (a node pool kept for runs).
	Tolerations []Toleration `yaml:"tolerations"`
	// Kubeconfig, if set, is used instead of the Pod's own service account
	// (running a worker outside the cluster, e.g. for local tests).
	Kubeconfig string `yaml:"kubeconfig"`
	// ProbeTargets are host:port pairs a run Pod must not reach, besides the
	// API server and the internet (the Helm chart adds the worker itself).
	ProbeTargets []string `yaml:"probe_targets"`
}

// Toleration is a Kubernetes toleration for run Pods.
type Toleration struct {
	Key      string `yaml:"key"`
	Operator string `yaml:"operator"` // Equal (default) or Exists
	Value    string `yaml:"value"`
	Effect   string `yaml:"effect"` // NoSchedule, PreferNoSchedule, NoExecute, or "" for all
}

// Limits are the per-run resource limits.
type Limits struct {
	Memory  Size          `yaml:"memory"`
	CPUs    float64       `yaml:"cpus"`
	PIDs    int64         `yaml:"pids"`
	TmpSize Size          `yaml:"tmp_size"`
	ShmSize Size          `yaml:"shm_size"`
	LogCap  Size          `yaml:"log_cap"`
	Timeout time.Duration `yaml:"timeout"`
}

// Uploads are the storage limits.
type Uploads struct {
	MaxFile  Size `yaml:"max_file"`
	MaxSpace Size `yaml:"max_space"`
}

// Quotas are per-owner limits. An owner is a caller ID: a basic user's name,
// or a token's "<prefix>:<claim>..." (see auth.Principal).
type Quotas struct {
	// MaxSpaces bounds how many spaces one owner may have (0: no limit).
	MaxSpaces int `yaml:"max_spaces"`
	// Owners raise or lower the limits for particular owners; a zero field
	// keeps the default (max_spaces, uploads.max_space).
	Owners []OwnerQuota `yaml:"owners"`
}

// OwnerQuota is one owner's limits.
type OwnerQuota struct {
	Owner         string `yaml:"owner"`
	MaxSpaces     int    `yaml:"max_spaces"`
	MaxSpaceBytes Size   `yaml:"max_space"`
}

// Runs configures the run queue and lifecycle.
type Runs struct {
	Concurrency     int           `yaml:"concurrency"`
	Retention       time.Duration `yaml:"retention"`
	StopGrace       time.Duration `yaml:"stop_grace"`
	DependencyCheck string        `yaml:"dependency_check"` // warn | reject
	MaxParameters   Size          `yaml:"max_parameters"`   // a run's parameters, as JSON
	MaxOutput       Size          `yaml:"max_output"`       // the value a notebook returns with nbsandbox.output
	EventRetention  time.Duration `yaml:"event_retention"`  // how long status events can be replayed
	// IdempotencyWindow is how long an Idempotency-Key returns the original
	// run (default 24h).
	IdempotencyWindow time.Duration `yaml:"idempotency_window"`
	// Lease is how long a worker's hold on a run lasts unless renewed (every
	// third of it); a run whose worker went away is taken over after this
	// (default 30s).
	Lease time.Duration `yaml:"lease"`
	// CleanupEvery is how often finished runs, events and keys past their
	// retention are removed (default 1h). Tests shorten it with the
	// retention settings so every cleanup path runs many times.
	CleanupEvery time.Duration `yaml:"cleanup_every"`
}

// The roles a process can have.
const (
	RoleAll    = "all"
	RoleAPI    = "api"
	RoleWorker = "worker"
)

// Executes reports whether this process runs notebooks.
func (s Settings) Executes() bool { return s.Role != RoleAPI }

// Serves reports whether this process serves the API and UI.
func (s Settings) Serves() bool { return s.Role != RoleWorker }

// Functions configures publishing notebooks as functions.
type Functions struct {
	// RequireSchemas refuses to publish a function whose contract lacks
	// input and output schemas (default false: the contract may be inferred).
	RequireSchemas bool `yaml:"require_schemas"`
}

// Default returns the built-in defaults.
func Default() Settings {
	return Settings{
		Listen:      ":8080",
		Role:        "all",
		Backend:     "docker",
		DataDir:     "/srv/nbsandbox",
		DatabaseURL: "postgres://nbsandbox@localhost:5432/nbsandbox?sslmode=disable",
		Storage:     Storage{Kind: "s3", S3: s3store.Config{Endpoint: "localhost:9000", Bucket: "nbsandbox"}},
		UsersFile:   "/etc/nbsandbox/users.yaml",
		LogLevel:    "info",
		Docker:      Docker{Image: "nbsandbox-runner:dev"},
		Limits: Limits{
			Memory:  2 * GiB,
			CPUs:    1,
			PIDs:    256,
			TmpSize: 256 * MiB,
			ShmSize: 64 * MiB,
			LogCap:  10 * MiB,
			Timeout: 10 * time.Minute,
		},
		Uploads: Uploads{MaxFile: 50 * MiB, MaxSpace: 500 * MiB},
		Runs: Runs{
			Concurrency:       2,
			Retention:         30 * 24 * time.Hour,
			StopGrace:         5 * time.Second,
			DependencyCheck:   "warn",
			MaxParameters:     1 * MiB,
			MaxOutput:         1 * MiB,
			EventRetention:    24 * time.Hour,
			IdempotencyWindow: 24 * time.Hour,
			CleanupEvery:      time.Hour,
			Lease:             30 * time.Second,
		},
		ShutdownGrace: 60 * time.Second,
		UI:            UI{Enabled: false, Listen: "127.0.0.1:8090"},
	}
}

// Load reads the YAML file at path (if path is not empty) over the defaults,
// applies environment overrides, and validates the result. All validation
// problems are returned together.
func Load(path string, getenv func(string) string) (Settings, error) {
	s := Default()
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
		if err != nil {
			return s, fmt.Errorf("read settings: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true) // a misspelled key is an error, not silently ignored
		// An empty file decodes to io.EOF and means "all defaults".
		if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
			return s, fmt.Errorf("parse settings %s: %w", path, err)
		}
	}
	if err := applyAuthEnv(&s, getenv); err != nil {
		return s, err
	}
	if err := applyEnv(&s, getenv); err != nil {
		return s, err
	}
	// The data folder (and the users file, often inside it) is bind-mounted
	// at the same path in containers, and Docker cleans a mount's path:
	// ".../work/../data" is mounted at ".../data". Use the same, clean paths.
	for _, p := range []*string{&s.DataDir, &s.UsersFile} {
		if *p != "" {
			*p = filepath.Clean(*p)
		}
	}
	return s, s.Validate()
}

// Authentication overrides: NBSANDBOX_AUTH_BASIC (true/false) and
// NBSANDBOX_OIDC_ISSUER, _AUDIENCE, _JWKS_URL, _ID_PREFIX, _ID_CLAIMS (comma
// separated). Setting the issuer turns OIDC on.
func applyAuthEnv(s *Settings, getenv func(string) string) error {
	if v := getenv("NBSANDBOX_AUTH_BASIC"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_AUTH_BASIC: %w", err)
		}
		s.Auth.Basic = &b
	}
	if v := getenv("NBSANDBOX_OIDC_ISSUER"); v != "" && s.Auth.OIDC == nil {
		s.Auth.OIDC = &auth.OIDCConfig{}
	}
	o := s.Auth.OIDC
	if o == nil {
		return nil
	}
	for name, p := range map[string]*string{
		"NBSANDBOX_OIDC_ISSUER": &o.Issuer, "NBSANDBOX_OIDC_AUDIENCE": &o.Audience,
		"NBSANDBOX_OIDC_JWKS_URL": &o.JWKSURL, "NBSANDBOX_OIDC_ID_PREFIX": &o.IDPrefix,
	} {
		if v := getenv(name); v != "" {
			*p = v
		}
	}
	if v := getenv("NBSANDBOX_OIDC_ID_CLAIMS"); v != "" {
		o.IDClaims = nil
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" {
				o.IDClaims = append(o.IDClaims, c)
			}
		}
	}
	return nil
}

// Environment overrides. DOCKER_HOST is honoured when docker.host is unset.
func applyEnv(s *Settings, getenv func(string) string) error {
	str := map[string]*string{
		"NBSANDBOX_LISTEN":                  &s.Listen,
		"NBSANDBOX_DATA_DIR":                &s.DataDir,
		"NBSANDBOX_DATABASE_URL":            &s.DatabaseURL,
		"NBSANDBOX_STORAGE":                 &s.Storage.Kind,
		"NBSANDBOX_S3_ENDPOINT":             &s.Storage.S3.Endpoint,
		"NBSANDBOX_S3_BUCKET":               &s.Storage.S3.Bucket,
		"NBSANDBOX_S3_REGION":               &s.Storage.S3.Region,
		"NBSANDBOX_S3_PREFIX":               &s.Storage.S3.Prefix,
		"NBSANDBOX_S3_ACCESS_KEY":           &s.Storage.S3.AccessKey,
		"NBSANDBOX_S3_SECRET_KEY":           &s.Storage.S3.SecretKey,
		"NBSANDBOX_AZURE_ACCOUNT_URL":       &s.Storage.Azure.AccountURL,
		"NBSANDBOX_AZURE_CONNECTION_STRING": &s.Storage.Azure.ConnectionString,
		"NBSANDBOX_AZURE_CONTAINER":         &s.Storage.Azure.Container,
		"NBSANDBOX_AZURE_PREFIX":            &s.Storage.Azure.Prefix,
		"NBSANDBOX_USERS_FILE":              &s.UsersFile,
		"NBSANDBOX_LOG_LEVEL":               &s.LogLevel,
		"NBSANDBOX_ROLE":                    &s.Role,
		"NBSANDBOX_BACKEND":                 &s.Backend,
		"NBSANDBOX_K8S_NAMESPACE":           &s.Kubernetes.Namespace,
		"NBSANDBOX_K8S_RUNTIME_CLASS":       &s.Kubernetes.RuntimeClass,
		"NBSANDBOX_KUBECONFIG":              &s.Kubernetes.Kubeconfig,
		"NBSANDBOX_WORKER_ID":               &s.WorkerID,
		"NBSANDBOX_DOCKER_HOST":             &s.Docker.Host,
		"NBSANDBOX_DOCKER_RUNTIME":          &s.Docker.Runtime,
		"NBSANDBOX_RUNNER_IMAGE":            &s.Docker.Image,
		"NBSANDBOX_UI_LISTEN":               &s.UI.Listen,
	}
	for k, p := range str {
		if v := getenv(k); v != "" {
			*p = v
		}
	}
	if s.Docker.Host == "" {
		s.Docker.Host = getenv("DOCKER_HOST")
	}
	if v := getenv("NBSANDBOX_UI_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_UI_ENABLED: %w", err)
		}
		s.UI.Enabled = b
	}
	durations := map[string]*time.Duration{
		"NBSANDBOX_RUN_TIMEOUT":        &s.Limits.Timeout,
		"NBSANDBOX_SHUTDOWN_GRACE":     &s.ShutdownGrace,
		"NBSANDBOX_RUN_RETENTION":      &s.Runs.Retention,
		"NBSANDBOX_RUN_LEASE":          &s.Runs.Lease,
		"NBSANDBOX_EVENT_RETENTION":    &s.Runs.EventRetention,
		"NBSANDBOX_IDEMPOTENCY_WINDOW": &s.Runs.IdempotencyWindow,
		"NBSANDBOX_CLEANUP_EVERY":      &s.Runs.CleanupEvery,
	}
	for k, p := range durations {
		if v := getenv(k); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*p = d
		}
	}
	for name, p := range map[string]*bool{
		"NBSANDBOX_S3_TLS":                 &s.Storage.S3.TLS,
		"NBSANDBOX_S3_CREATE_BUCKET":       &s.Storage.S3.CreateBucket,
		"NBSANDBOX_AZURE_CREATE_CONTAINER": &s.Storage.Azure.CreateContainer,
		"NBSANDBOX_REQUIRE_SCHEMAS":        &s.Functions.RequireSchemas,
	} {
		if v := getenv(name); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*p = b
		}
	}
	if v := getenv("NBSANDBOX_K8S_PROBE_TARGETS"); v != "" {
		s.Kubernetes.ProbeTargets = strings.Split(v, ",")
	}
	if v := getenv("NBSANDBOX_RUN_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_RUN_CONCURRENCY: %w", err)
		}
		s.Runs.Concurrency = n
	}
	return nil
}

// Validate returns every problem at once, joined.
func (s Settings) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if s.Listen == "" {
		add("listen: must not be empty")
	}
	switch s.Role {
	case RoleAll, RoleAPI, RoleWorker:
	default:
		add("role: must be all, api or worker, got %q", s.Role)
	}
	switch s.Backend {
	case "docker":
	case "kubernetes":
		if s.Kubernetes.Namespace == "" {
			add("kubernetes.namespace: required with backend kubernetes")
		}
		if s.Docker.Runtime != "" {
			add("docker.runtime: not used with backend kubernetes; set kubernetes.runtime_class")
		}
	default:
		add("backend: must be docker or kubernetes, got %q", s.Backend)
	}
	if s.Runs.Lease < 3*time.Second || s.Runs.Lease > 10*time.Minute {
		add("runs.lease: must be between 3s and 10m, got %s", s.Runs.Lease)
	}
	if u, err := url.Parse(s.DatabaseURL); s.DatabaseURL == "" || err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		add("database_url: must be a postgres:// URL with a host (set NBSANDBOX_DATABASE_URL)")
	}
	switch s.Storage.Kind {
	case "s3":
		if s.Storage.S3.Endpoint == "" || s.Storage.S3.Bucket == "" {
			add("storage.s3: endpoint and bucket are required")
		}
	case "azure":
		if s.Storage.Azure.Container == "" || (s.Storage.Azure.AccountURL == "" && s.Storage.Azure.ConnectionString == "") {
			add("storage.azure: container and account_url (or connection_string) are required")
		}
	default:
		add("storage.kind: must be s3 or azure, got %q", s.Storage.Kind)
	}
	if !filepath.IsAbs(s.DataDir) {
		add("data_dir: must be an absolute path, got %q", s.DataDir)
	}
	if s.UsersFile == "" && (s.Auth.BasicEnabled() || s.UI.Enabled) {
		add("users_file: must not be empty while basic auth or the web UI is on")
	}
	if !s.Auth.BasicEnabled() && s.Auth.OIDC == nil {
		add("auth: basic auth is off and no OIDC provider is set: nobody could sign in")
	}
	if s.Auth.OIDC != nil {
		if err := s.Auth.OIDC.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("log_level: must be debug, info, warn or error, got %q", s.LogLevel)
	}
	switch s.Docker.Runtime {
	case "", "runsc":
	default:
		add("docker.runtime: must be empty (runc) or \"runsc\", got %q", s.Docker.Runtime)
	}
	if s.Docker.Image == "" {
		add("docker.image: must not be empty")
	}
	if s.Docker.Host != "" {
		u, err := url.Parse(s.Docker.Host)
		if err != nil || (u.Scheme != "unix" && u.Scheme != "tcp" && u.Scheme != "npipe") {
			add("docker.host: must be a unix://, tcp:// or npipe:// URL, got %q", s.Docker.Host)
		}
	}
	if s.Limits.Memory < 128*MiB {
		add("limits.memory: must be at least 128MiB, got %s", s.Limits.Memory)
	}
	if s.Limits.CPUs <= 0 {
		add("limits.cpus: must be positive, got %v", s.Limits.CPUs)
	}
	if s.Limits.PIDs < 16 {
		add("limits.pids: must be at least 16, got %d", s.Limits.PIDs)
	}
	if s.Limits.TmpSize <= 0 || s.Limits.TmpSize >= s.Limits.Memory {
		add("limits.tmp_size: must be positive and below limits.memory (tmpfs counts toward memory), got %s", s.Limits.TmpSize)
	}
	if s.Limits.ShmSize <= 0 {
		add("limits.shm_size: must be positive, got %s", s.Limits.ShmSize)
	}
	if s.Limits.LogCap < 64*KiB {
		add("limits.log_cap: must be at least 64KiB, got %s", s.Limits.LogCap)
	}
	if s.Limits.Timeout < time.Second {
		add("limits.timeout: must be at least 1s, got %s", s.Limits.Timeout)
	}
	if s.Uploads.MaxFile <= 0 || s.Uploads.MaxSpace < s.Uploads.MaxFile {
		add("uploads: max_file must be positive and max_space at least max_file, got %s / %s", s.Uploads.MaxFile, s.Uploads.MaxSpace)
	}
	if s.Quotas.MaxSpaces < 0 {
		add("quotas.max_spaces: must be 0 (no limit) or more, got %d", s.Quotas.MaxSpaces)
	}
	seen := map[string]bool{}
	for i, o := range s.Quotas.Owners {
		switch {
		case o.Owner == "":
			add("quotas.owners[%d]: owner is required", i)
		case seen[o.Owner]:
			add("quotas.owners[%d]: owner %q is listed twice", i, o.Owner)
		case o.MaxSpaces < 0 || o.MaxSpaceBytes < 0 || (o.MaxSpaceBytes > 0 && o.MaxSpaceBytes < s.Uploads.MaxFile):
			add("quotas.owners[%d]: max_spaces must not be negative, and max_space must be 0 or at least uploads.max_file", i)
		}
		seen[o.Owner] = true
	}
	if s.Runs.Concurrency < 1 {
		add("runs.concurrency: must be at least 1, got %d", s.Runs.Concurrency)
	}
	if s.Runs.Retention < time.Minute {
		add("runs.retention: must be at least 1m, got %s", s.Runs.Retention)
	}
	if s.Runs.StopGrace < 0 || s.Runs.StopGrace > time.Minute {
		add("runs.stop_grace: must be between 0 and 1m, got %s", s.Runs.StopGrace)
	}
	if s.Runs.MaxParameters < 1*KiB || s.Runs.MaxParameters > 16*MiB {
		add("runs.max_parameters: must be between 1KiB and 16MiB, got %s", s.Runs.MaxParameters)
	}
	if s.Runs.MaxOutput < 1*KiB || s.Runs.MaxOutput > 64*MiB {
		add("runs.max_output: must be between 1KiB and 64MiB, got %s", s.Runs.MaxOutput)
	}
	if s.Runs.EventRetention < time.Minute {
		add("runs.event_retention: must be at least 1m, got %s", s.Runs.EventRetention)
	}
	if s.Runs.IdempotencyWindow < time.Minute {
		add("runs.idempotency_window: must be at least 1m, got %s", s.Runs.IdempotencyWindow)
	}
	if s.Runs.CleanupEvery < 10*time.Second || s.Runs.CleanupEvery > 24*time.Hour {
		add("runs.cleanup_every: must be between 10s and 24h, got %s", s.Runs.CleanupEvery)
	}
	switch s.Runs.DependencyCheck {
	case "warn", "reject":
	default:
		add("runs.dependency_check: must be warn or reject, got %q", s.Runs.DependencyCheck)
	}
	if s.UI.Enabled && s.UI.Listen == "" {
		add("ui.listen: must not be empty when ui.enabled is true")
	}
	if s.UI.Enabled && s.UI.Listen == s.Listen {
		add("ui.listen: must differ from listen (the UI needs its own port)")
	}
	if s.ShutdownGrace < 0 {
		add("shutdown_grace: must not be negative, got %s", s.ShutdownGrace)
	}
	return errors.Join(errs...)
}
