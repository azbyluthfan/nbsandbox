// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	"github.com/azbyluthfan/nbsandbox/internal/packages"
	"github.com/azbyluthfan/nbsandbox/internal/uiauth"
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
	// Audit records who did what (docs/operations.md#audit-log).
	Audit Audit `yaml:"audit"`
	// Packages builds a space's requirements.txt into a package set its
	// runs import (docs/packages.md).
	Packages Packages `yaml:"packages"`
	// Sessions are interactive sessions: a notebook's live kernel
	// (docs/sessions.md).
	Sessions Sessions `yaml:"sessions"`
	// Connections are data sources users register, which notebooks read
	// where the data lives (docs/plan-data-access.md).
	Connections Connections `yaml:"connections"`

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
	// LoginLimit slows down password guessing against basic auth.
	LoginLimit LoginLimit `yaml:"login_limit"`
	// TrustedProxies are the proxies (addresses, CIDRs, or host names such
	// as the Compose service "caddy") whose X-Forwarded-For names the client:
	// Caddy, an ingress. Without them, the
	// connection's peer is the client, and behind a proxy the per-address
	// login limit sees only the proxy.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// LoginLimit: each username and each client address may fail to sign in
// Failures times; then one more failure is allowed per Refill, and attempts
// beyond get 429 without the password being checked. A successful sign-in
// restores the username's allowance. Failures 0 turns the limit off.
type LoginLimit struct {
	Failures *int          `yaml:"failures"`
	Refill   time.Duration `yaml:"refill"`
	// PerAddress also limits failures per client address (password
	// spraying). Default: on when trusted_proxies is set (then the address
	// is the client's), off otherwise, since behind an unnamed proxy every
	// client would share the proxy's address and one guesser would lock
	// everyone out. Set true for a server that clients reach directly.
	PerAddress *bool `yaml:"per_address"`
}

// ByAddress reports whether failures are also limited per client address.
func (a Auth) ByAddress() bool {
	if a.LoginLimit.PerAddress != nil {
		return *a.LoginLimit.PerAddress
	}
	return len(a.TrustedProxies) > 0
}

// Allowance is the configured failures (default 5) and refill (default 1m).
func (l LoginLimit) Allowance() (int, time.Duration) {
	f, r := 5, l.Refill
	if l.Failures != nil {
		f = *l.Failures
	}
	if r <= 0 {
		r = time.Minute
	}
	return f, r
}

// BasicEnabled reports whether basic auth is on (the default).
func (a Auth) BasicEnabled() bool { return a.Basic == nil || *a.Basic }

// UI is the optional web UI, on its own listener. Off by default. Without
// sign-in (auth: none) it acts as any users.yaml user without a password, a
// development tool that listens on loopback only; with auth basic or oidc,
// people sign in and it can be served to a network.
type UI struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	// Auth is how people sign in: none, basic or oidc.
	Auth string `yaml:"auth"`
	// PublicURL is the address browsers open (required with basic or oidc).
	PublicURL string `yaml:"public_url"`
	// NoLoginBeyondLoopback lets auth: none listen beyond loopback. Only for
	// a container whose port is published on the host's loopback (the dev
	// Compose stack): anyone who reaches the port can act as any user.
	NoLoginBeyondLoopback bool      `yaml:"no_login_beyond_loopback"`
	Session               UISession `yaml:"session"`
	OIDC                  UIOIDC    `yaml:"oidc"`
}

// Packages configures custom packages per space.
// Sessions configures interactive sessions. A session runs with the run
// limits (limits:), for longer.
type Sessions struct {
	Enabled bool `yaml:"enabled"`
	// Idle ends a session nobody uses: no cell running and no browser
	// connected, for this long.
	Idle time.Duration `yaml:"idle"`
	// Max ends a session in any case, this long after it started.
	Max time.Duration `yaml:"max"`
	// PerUser bounds a user's open sessions (0: no limit).
	PerUser int `yaml:"per_user"`
	// PerWorker bounds the sessions a worker holds, beside runs.concurrency.
	PerWorker int `yaml:"per_worker"`
	// WriteBack is how often a read-write session's file changes are
	// written to the space (also on request, and when it ends).
	WriteBack time.Duration `yaml:"write_back"`
	// MaxMessage bounds one output a session sends (larger ones become a
	// notice).
	MaxMessage Size `yaml:"max_message"`
	// Retention keeps ended sessions' records this long.
	Retention time.Duration `yaml:"retention"`

	// With API and worker processes (role api or worker), API nodes reach
	// the worker holding a session on its internal listener.
	//
	// Listen is a worker's internal listener for API nodes (":8091").
	Listen string `yaml:"listen"`
	// Address is how API nodes reach this worker's Listen (host:port);
	// default: the worker's host name and Listen's port.
	Address string `yaml:"address"`
	// Secret authenticates API nodes to workers: the same long random value
	// on every process (NBSANDBOX_SESSIONS_SECRET). Never logged.
	Secret string `yaml:"secret"`
}

// Connections configures connections: a user's data sources (a database,
// object storage, an API), named in their spaces and read where the data
// lives. Their credentials are kept by a secret provider, never in
// nbsandbox's own records.
type Connections struct {
	Enabled bool `yaml:"enabled"`
	// Allowed lists where connections may point: host names, "*.domain"
	// patterns and CIDR ranges. Empty: nowhere. Loopback, link-local (cloud
	// metadata) and unspecified addresses are refused whatever it says.
	Allowed []string `yaml:"allowed"`
	// Secrets is where connections' credentials are kept.
	Secrets ConnectionSecrets `yaml:"secrets"`
}

// Secret providers for connections.
const (
	SecretsBuiltin    = "builtin"    // encrypted in nbsandbox's database; development only
	SecretsKubernetes = "kubernetes" // Kubernetes Secrets in the runs namespace
)

// ConnectionSecrets configures where connections' credentials are kept.
type ConnectionSecrets struct {
	// Provider is builtin (development) or kubernetes.
	Provider string `yaml:"provider"`
	// MasterKey encrypts the built-in store: 32 random bytes, base64
	// (NBSANDBOX_CONNECTIONS_MASTER_KEY). Never logged. Losing it loses
	// every stored secret.
	MasterKey string `yaml:"master_key"`
	// Namespace holds the Kubernetes Secrets nbsandbox writes, and those
	// connections refer to: the runs namespace by default, since a Pod can
	// mount only its own namespace's Secrets.
	Namespace string `yaml:"namespace"`
}

// SessionsOn reports whether this process runs interactive sessions:
// turned on, and, with API and worker processes, given the secret they
// share.
func (s Settings) SessionsOn() bool {
	return s.Sessions.Enabled && (s.Role == RoleAll || s.Role == "" || s.Sessions.Secret != "")
}

// SessionAddress is where API nodes reach this worker's sessions.
func (s Sessions) SessionAddress(host string) string {
	if s.Address != "" {
		return s.Address
	}
	_, port, err := net.SplitHostPort(s.Listen)
	if err != nil || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

type Packages struct {
	Enabled bool `yaml:"enabled"`
	// IndexURL is the package index builds use (pip's --index-url).
	IndexURL string `yaml:"index_url"`
	// Allowed lists the distributions spaces may install (dependencies
	// included); empty allows any.
	Allowed []string `yaml:"allowed"`
	// MaxSize bounds a set, unpacked.
	MaxSize Size `yaml:"max_size"`
	// BuildTimeout bounds one build.
	BuildTimeout time.Duration `yaml:"build_timeout"`
	// Retention removes sets nobody used for this long.
	Retention time.Duration `yaml:"retention"`
	// CacheSize bounds the sets a worker keeps unpacked on its disk.
	CacheSize Size `yaml:"cache_size"`
	// BuilderImage has the runner's packages and pip (make runner-image).
	BuilderImage string `yaml:"builder_image"`
	// Network is the Docker network of build containers: the only
	// containers with network access.
	Network string `yaml:"network"`
}

// Audit configures the audit log.
type Audit struct {
	Enabled   bool          `yaml:"enabled"`
	Retention time.Duration `yaml:"retention"`
	Downloads bool          `yaml:"downloads"` // also record downloads of files and run outputs
}

// UISession bounds a signed-in session.
type UISession struct {
	Idle time.Duration `yaml:"idle"` // without a request
	Max  time.Duration `yaml:"max"`  // in any case
}

// UIOIDC is the UI's client at the identity provider (auth.oidc).
type UIOIDC struct {
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
}

// SignIn is the UI's sign-in configuration.
func (u UI) SignIn() uiauth.Config {
	return uiauth.Config{Mode: uiauth.Mode(u.Auth), PublicURL: u.PublicURL, Idle: u.Session.Idle, Max: u.Session.Max,
		ClientID: u.OIDC.ClientID, ClientSecret: u.OIDC.ClientSecret, Scopes: u.OIDC.Scopes}
}

// IsLoopback reports whether a listen address is on loopback only.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		Audit:         Audit{Enabled: true, Retention: 90 * 24 * time.Hour, Downloads: true},
		Packages: Packages{Enabled: false, IndexURL: "https://pypi.org/simple", Allowed: []string{}, MaxSize: GiB,
			BuildTimeout: 10 * time.Minute, Retention: 30 * 24 * time.Hour, CacheSize: 20 * GiB,
			BuilderImage: "nbsandbox-runner-builder:dev", Network: "bridge"},
		Sessions: Sessions{Enabled: true, Idle: 30 * time.Minute, Max: 8 * time.Hour, PerUser: 2, PerWorker: 4,
			WriteBack: 5 * time.Minute, MaxMessage: 2 * MiB, Retention: 7 * 24 * time.Hour, Listen: ":8091"},
		Connections: Connections{Enabled: false, Allowed: []string{}, Secrets: ConnectionSecrets{Provider: SecretsBuiltin}},
		UI: UI{Enabled: false, Listen: "127.0.0.1:8090", Auth: "none",
			Session: UISession{Idle: time.Hour, Max: 12 * time.Hour},
			OIDC: UIOIDC{Scopes: []string{"openid", "profile", auth.ScopeRead, auth.ScopeSpacesWrite, auth.ScopeFilesWrite,
				auth.ScopeRuns, auth.ScopeFunctionsInvoke, auth.ScopeFunctionsPublish, auth.ScopeConnections}}},
		Auth: Auth{LoginLimit: LoginLimit{Failures: new5(), Refill: time.Minute}, TrustedProxies: []string{}},
	}
}

func new5() *int { n := 5; return &n }

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

// Authentication overrides: NBSANDBOX_AUTH_BASIC (true/false),
// NBSANDBOX_TRUSTED_PROXIES (comma separated), NBSANDBOX_LOGIN_LIMIT_FAILURES,
// and NBSANDBOX_OIDC_ISSUER, _AUDIENCE, _JWKS_URL, _ID_PREFIX, _ID_CLAIMS
// (comma separated), _INTROSPECTION_CLIENT_ID, _INTROSPECTION_CLIENT_SECRET,
// _INTROSPECTION_CACHE. Setting the issuer turns OIDC on.
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
	if v := getenv("NBSANDBOX_TRUSTED_PROXIES"); v != "" {
		s.Auth.TrustedProxies = strings.Split(v, ",")
	}
	if v := getenv("NBSANDBOX_LOGIN_LIMIT_FAILURES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_LOGIN_LIMIT_FAILURES: %w", err)
		}
		s.Auth.LoginLimit.Failures = &n
	}
	o := s.Auth.OIDC
	if o == nil {
		return nil
	}
	if v := getenv("NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET"); v != "" {
		if o.Introspection == nil {
			o.Introspection = &auth.IntrospectionConfig{}
		}
		o.Introspection.ClientSecret = v
	}
	if v := getenv("NBSANDBOX_OIDC_INTROSPECTION_CLIENT_ID"); v != "" {
		if o.Introspection == nil {
			o.Introspection = &auth.IntrospectionConfig{}
		}
		o.Introspection.ClientID = v
	}
	if v := getenv("NBSANDBOX_OIDC_INTROSPECTION_CACHE"); v != "" && o.Introspection != nil {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_OIDC_INTROSPECTION_CACHE: %w", err)
		}
		o.Introspection.Cache = d
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
		"NBSANDBOX_UI_AUTH":                 &s.UI.Auth,
		"NBSANDBOX_PACKAGES_INDEX_URL":      &s.Packages.IndexURL,
		"NBSANDBOX_PACKAGES_BUILDER_IMAGE":  &s.Packages.BuilderImage,
		"NBSANDBOX_PACKAGES_NETWORK":        &s.Packages.Network,
		"NBSANDBOX_UI_PUBLIC_URL":           &s.UI.PublicURL,
		"NBSANDBOX_UI_OIDC_CLIENT_ID":       &s.UI.OIDC.ClientID,
		"NBSANDBOX_UI_OIDC_CLIENT_SECRET":   &s.UI.OIDC.ClientSecret,
		"NBSANDBOX_SESSIONS_LISTEN":         &s.Sessions.Listen,
		"NBSANDBOX_SESSIONS_ADDRESS":        &s.Sessions.Address,
		"NBSANDBOX_SESSIONS_SECRET":         &s.Sessions.Secret,
		"NBSANDBOX_CONNECTIONS_PROVIDER":    &s.Connections.Secrets.Provider,
		"NBSANDBOX_CONNECTIONS_MASTER_KEY":  &s.Connections.Secrets.MasterKey,
		"NBSANDBOX_CONNECTIONS_NAMESPACE":   &s.Connections.Secrets.Namespace,
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
	if v := getenv("NBSANDBOX_UI_NO_LOGIN_BEYOND_LOOPBACK"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("NBSANDBOX_UI_NO_LOGIN_BEYOND_LOOPBACK: %w", err)
		}
		s.UI.NoLoginBeyondLoopback = b
	}
	if v := getenv("NBSANDBOX_CONNECTIONS_ALLOWED"); v != "" {
		s.Connections.Allowed = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if v := getenv("NBSANDBOX_PACKAGES_ALLOWED"); v != "" {
		s.Packages.Allowed = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if v := getenv("NBSANDBOX_UI_OIDC_SCOPES"); v != "" {
		s.UI.OIDC.Scopes = strings.Fields(strings.ReplaceAll(v, ",", " "))
	}
	durations := map[string]*time.Duration{
		"NBSANDBOX_RUN_TIMEOUT":        &s.Limits.Timeout,
		"NBSANDBOX_UI_SESSION_IDLE":    &s.UI.Session.Idle,
		"NBSANDBOX_AUDIT_RETENTION":    &s.Audit.Retention,
		"NBSANDBOX_PACKAGES_TIMEOUT":   &s.Packages.BuildTimeout,
		"NBSANDBOX_UI_SESSION_MAX":     &s.UI.Session.Max,
		"NBSANDBOX_SHUTDOWN_GRACE":     &s.ShutdownGrace,
		"NBSANDBOX_RUN_RETENTION":      &s.Runs.Retention,
		"NBSANDBOX_RUN_LEASE":          &s.Runs.Lease,
		"NBSANDBOX_EVENT_RETENTION":    &s.Runs.EventRetention,
		"NBSANDBOX_IDEMPOTENCY_WINDOW": &s.Runs.IdempotencyWindow,
		"NBSANDBOX_CLEANUP_EVERY":      &s.Runs.CleanupEvery,
		"NBSANDBOX_SESSIONS_IDLE":      &s.Sessions.Idle,
		"NBSANDBOX_SESSIONS_MAX":       &s.Sessions.Max,
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
		"NBSANDBOX_AUDIT_ENABLED":          &s.Audit.Enabled,
		"NBSANDBOX_PACKAGES_ENABLED":       &s.Packages.Enabled,
		"NBSANDBOX_AUDIT_DOWNLOADS":        &s.Audit.Downloads,
		"NBSANDBOX_SESSIONS_ENABLED":       &s.Sessions.Enabled,
		"NBSANDBOX_CONNECTIONS_ENABLED":    &s.Connections.Enabled,
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
	for name, p := range map[string]*int{
		"NBSANDBOX_SESSIONS_PER_USER":   &s.Sessions.PerUser,
		"NBSANDBOX_SESSIONS_PER_WORKER": &s.Sessions.PerWorker,
	} {
		if v := getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*p = n
		}
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
	if s.UsersFile == "" && (s.Auth.BasicEnabled() || (s.UI.Enabled && s.UI.Auth != string(uiauth.ModeOIDC))) {
		add("users_file: must not be empty while basic auth or the web UI is on")
	}
	if _, err := auth.NewTrustedProxies(s.Auth.TrustedProxies); err != nil {
		add("auth.trusted_proxies: %v", err)
	}
	if f, _ := s.Auth.LoginLimit.Allowance(); f < 0 {
		add("auth.login_limit.failures: must not be negative (0 turns the limit off)")
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
	if ss := s.Sessions; ss.Enabled {
		if ss.Idle < 10*time.Second || ss.Max < ss.Idle || ss.Max > 7*24*time.Hour {
			add("sessions: idle must be at least 10s, and max between idle and 168h, got %s and %s", ss.Idle, ss.Max)
		}
		if ss.PerUser < 0 || ss.PerWorker < 1 {
			add("sessions: per_user must be 0 (no limit) or more, and per_worker at least 1, got %d and %d", ss.PerUser, ss.PerWorker)
		}
		if ss.WriteBack < 10*time.Second || ss.WriteBack > time.Hour {
			add("sessions.write_back: must be between 10s and 1h, got %s", ss.WriteBack)
		}
		if ss.MaxMessage < 64*KiB || ss.MaxMessage > 64*MiB {
			add("sessions.max_message: must be between 64KiB and 64MiB, got %s", ss.MaxMessage)
		}
		if ss.Retention < time.Minute {
			add("sessions.retention: must be at least 1m, got %s", ss.Retention)
		}
		if s.Role == RoleAPI || s.Role == RoleWorker {
			// No secret: sessions stay off on this process (SessionsOn), so a
			// cluster upgraded without one still starts.
			if ss.Secret != "" && len(ss.Secret) < 16 {
				add("sessions.secret: must be at least 16 characters (the same long random value on every process)")
			}
			if _, _, err := net.SplitHostPort(ss.Listen); s.Role == RoleWorker && err != nil {
				add("sessions.listen: a worker's internal listener for API nodes, host:port or :port, got %q", ss.Listen)
			}
		}
	}
	if c := s.Connections; c.Enabled {
		switch c.Secrets.Provider {
		case SecretsBuiltin:
			if k, err := base64.StdEncoding.DecodeString(c.Secrets.MasterKey); err != nil || len(k) != 32 {
				add("connections.secrets.master_key: the built-in secret store needs 32 random bytes, base64 (openssl rand -base64 32)")
			}
		case SecretsKubernetes:
			if s.Backend != "kubernetes" {
				add("connections.secrets.provider: kubernetes needs the Kubernetes backend")
			}
		default:
			add("connections.secrets.provider: must be %s or %s, got %q", SecretsBuiltin, SecretsKubernetes, c.Secrets.Provider)
		}
		for _, a := range c.Allowed {
			if err := ValidAllowedEntry(a); err != nil {
				add("connections.allowed: %v", err)
			}
		}
	}
	if p := s.Packages; p.Enabled {
		if s.Backend == "kubernetes" {
			add("packages.enabled: custom packages run on the docker backend only, for now")
		}
		if u, err := url.Parse(p.IndexURL); err != nil || u.Scheme != "https" || u.Host == "" {
			add("packages.index_url: must be an https URL, got %q", p.IndexURL)
		}
		if strings.TrimSpace(p.BuilderImage) == "" || strings.TrimSpace(p.Network) == "" {
			add("packages: builder_image and network must be set")
		}
	}
	if p := s.Packages; p.MaxSize < MiB || p.MaxSize > 50*GiB || p.CacheSize < p.MaxSize {
		add("packages: max_size must be between 1MiB and 50GiB, and cache_size at least max_size (got %s, %s)", p.MaxSize, p.CacheSize)
	}
	if p := s.Packages; p.BuildTimeout < time.Minute || p.BuildTimeout > 2*time.Hour || p.Retention < 24*time.Hour {
		add("packages: build_timeout must be between 1m and 2h, and retention at least 24h (got %s, %s)", p.BuildTimeout, p.Retention)
	}
	for _, a := range s.Packages.Allowed {
		if _, err := packages.Parse([]byte(a)); err != nil || strings.ContainsAny(a, "=<>!~;[ ") {
			add("packages.allowed: %q is not a distribution name", a)
		}
	}
	if s.Audit.Retention < 24*time.Hour || s.Audit.Retention > 10*365*24*time.Hour {
		add("audit.retention: must be between 24h and 87600h (10 years), got %s", s.Audit.Retention)
	}
	if s.UI.Enabled && s.UI.Listen == "" {
		add("ui.listen: must not be empty when ui.enabled is true")
	}
	if s.UI.Enabled && s.UI.Listen == s.Listen {
		add("ui.listen: must differ from listen (the UI needs its own port)")
	}
	if s.UI.Enabled {
		if err := s.UI.SignIn().Validate("ui"); err != nil {
			errs = append(errs, err)
		}
		switch uiauth.Mode(s.UI.Auth) {
		case uiauth.ModeNone:
			if !IsLoopback(s.UI.Listen) && !s.UI.NoLoginBeyondLoopback {
				add("ui.listen: without sign-in (ui.auth: none) anyone who reaches the UI acts as any user, so it listens on loopback only, "+
					"got %q; set ui.auth to basic or oidc", s.UI.Listen)
			}
		case uiauth.ModeOIDC:
			if s.Auth.OIDC == nil {
				add("ui.auth: oidc signs in with the identity provider in auth.oidc, which is not set")
			}
		}
	}
	if s.ShutdownGrace < 0 {
		add("shutdown_grace: must not be negative, got %s", s.ShutdownGrace)
	}
	return errors.Join(errs...)
}

// ValidAllowedEntry checks one connections.allowed entry: a CIDR range
// ("10.0.0.0/8"), a host name ("db.internal") or a "*.domain" pattern.
func ValidAllowedEntry(e string) error {
	if strings.Contains(e, "/") {
		if _, err := netip.ParsePrefix(e); err != nil {
			return fmt.Errorf("%q is not a CIDR range", e)
		}
		return nil
	}
	h := strings.TrimPrefix(e, "*.")
	if h == "" || strings.ContainsAny(h, "*/:@ ") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return fmt.Errorf("%q is not a host name, *.domain pattern or CIDR range", e)
	}
	return nil
}
