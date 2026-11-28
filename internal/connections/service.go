// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Package connections manages connections: a user's data sources, named in
// their spaces per environment, read where the data lives
// (docs/plan-data-access.md). It decides what may be created and bound;
// credentials go to a secret provider and never come back out.
package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/secrets"
	"github.com/azbyluthfan/nbsandbox/internal/spaces"
	"github.com/azbyluthfan/nbsandbox/internal/store"
)

// Limits on a space's names and environments.
const (
	MaxEnvironments = 10
	MaxNames        = 50
	MaxSecret       = 64 << 10
)

var (
	// ErrNotFound: no such connection (of the caller's) or space.
	ErrNotFound = errors.New("not found")
	// ErrNameTaken: the caller already has a connection by that name.
	ErrNameTaken = store.ErrConnectionName
	// ErrInUse: a space binds the connection.
	ErrInUse = store.ErrConnectionInUse
)

// InvalidError is a request that can't be accepted as given (400).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

var (
	connectionName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	identifier     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`) // a binding or environment name: an SQL catalog name
)

// Deps are what the service needs.
type Deps struct {
	DB      *store.DB
	Secrets secrets.Provider
	Allowed Allowed
	// Fetcher makes runs' fetchers (mediated reads).
	Fetcher FetcherSettings
	Log     *slog.Logger
	Now     func() time.Time // default time.Now
}

// Service manages connections and what spaces bind.
type Service struct{ d Deps }

// New returns the service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d}
}

// SecretInput is a credential: its value (stored by nbsandbox: managed), a
// reference to one the provider already holds, or none. At most one of
// Value and Ref.
type SecretInput struct {
	Value *string
	Ref   *string
	None  bool
}

// Input creates a connection.
type Input struct {
	Name, Type string
	Settings   map[string]any
	Secret     SecretInput
}

// Patch changes a connection; nil fields stay as they are. Settings, when
// given, replace them all; Secret, when given, replaces the credential.
type Patch struct {
	Name     *string
	Settings map[string]any
	Secret   *SecretInput
}

// SecretView describes a credential without revealing it.
type SecretView struct {
	Mode     string `json:"mode"` // none, managed or referenced
	Provider string `json:"provider,omitempty"`
	Ref      string `json:"ref,omitempty"` // a referenced secret's name#key
}

// View is a connection as callers see it: never its secret's value.
type View struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Settings  map[string]any `json:"settings"`
	Secret    SecretView     `json:"secret"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	UsedBy    []Use          `json:"used_by,omitempty"`
}

// Use is a place a connection is bound.
type Use struct {
	SpaceID     string `json:"space_id"`
	SpaceName   string `json:"space_name"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
}

func view(c store.Connection) View {
	v := View{ID: c.ID, Name: c.Name, Type: c.Type, Settings: map[string]any{}, CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
		Secret: SecretView{Mode: c.SecretMode, Provider: c.SecretProvider}}
	_ = json.Unmarshal([]byte(c.Settings), &v.Settings)
	if c.SecretMode == store.SecretReferenced {
		v.Secret.Ref = c.SecretRef
	}
	if c.SecretMode == store.SecretNone {
		v.Secret.Provider = ""
	}
	return v
}

// check validates a connection's name, type and settings (normalised) and
// where it points.
func (s *Service) check(name, typ string, settings map[string]any) (map[string]any, error) {
	if !connectionName.MatchString(name) {
		return nil, invalid("name must start with a lowercase letter and have only a-z, 0-9, - and _ (at most 63), got %q", name)
	}
	norm, err := normalize(typ, settings)
	if err != nil {
		return nil, err
	}
	host, err := endpoint(typ, norm)
	if err != nil {
		return nil, err
	}
	for _, h := range []string{host, tokenHost(typ, norm)} {
		if h != "" && !s.d.Allowed.Permits(h) {
			return nil, invalid("connections may not point to %s: the operator allows only the hosts in connections.allowed (never loopback or link-local addresses)", h)
		}
	}
	return norm, nil
}

// putSecret stores or checks a credential for connection id.
func (s *Service) putSecret(ctx context.Context, id, typ string, settings map[string]any, in SecretInput) (mode, provider, ref string, err error) {
	needed, forbidden, is := secretRule(typ, settings)
	if forbidden && (in.Value != nil || in.Ref != nil) {
		return "", "", "", invalid("%s connections with auth %s take no secret", typ, str(settings, "auth"))
	}
	switch {
	case in.Value != nil && in.Ref != nil, in.None && (in.Value != nil || in.Ref != nil):
		return "", "", "", invalid("give the secret's value or a reference to it, not both")
	case in.Value != nil:
		if *in.Value == "" || len(*in.Value) > MaxSecret {
			return "", "", "", invalid("the secret must be 1 byte to %d KiB", MaxSecret>>10)
		}
		if s.d.Secrets == nil {
			return "", "", "", invalid("no secret provider is configured")
		}
		ref, err := s.d.Secrets.Store(ctx, id, []byte(*in.Value))
		if err != nil {
			return "", "", "", err
		}
		return store.SecretManaged, s.d.Secrets.Name(), ref, nil
	case in.Ref != nil:
		if s.d.Secrets == nil {
			return "", "", "", invalid("no secret provider is configured")
		}
		if err := s.d.Secrets.Check(ctx, *in.Ref); err != nil {
			if errors.Is(err, secrets.ErrNoReferences) || errors.Is(err, secrets.ErrBadReference) {
				return "", "", "", invalid("%v", err)
			}
			return "", "", "", err
		}
		return store.SecretReferenced, s.d.Secrets.Name(), *in.Ref, nil
	default:
		if needed {
			return "", "", "", invalid("%s connections need a secret: %s", typ, is)
		}
		return store.SecretNone, "", "", nil
	}
}

// dropSecret removes a managed credential; failures are logged (an orphan
// in the provider), not returned: the connection's record is what counts.
func (s *Service) dropSecret(ctx context.Context, mode, ref string) {
	if mode != store.SecretManaged || s.d.Secrets == nil {
		return
	}
	if err := s.d.Secrets.Remove(context.WithoutCancel(ctx), ref); err != nil {
		s.d.Log.Warn("remove a connection's secret", "ref", ref, "error", err)
	}
}

// Create records a new connection of owner's.
func (s *Service) Create(ctx context.Context, owner string, in Input) (View, error) {
	norm, err := s.check(in.Name, in.Type, in.Settings)
	if err != nil {
		return View{}, err
	}
	id := spaces.NewID()
	mode, provider, ref, err := s.putSecret(ctx, id, in.Type, norm, in.Secret)
	if err != nil {
		return View{}, err
	}
	b, _ := json.Marshal(norm)
	now := s.d.Now()
	c := store.Connection{ID: id, Owner: owner, Name: in.Name, Type: in.Type, Settings: string(b),
		SecretMode: mode, SecretProvider: provider, SecretRef: ref, CreatedAt: now, UpdatedAt: now}
	if err := s.d.DB.InsertConnection(ctx, c); err != nil {
		s.dropSecret(ctx, mode, ref)
		return View{}, err
	}
	return view(c), nil
}

// List returns owner's connections.
func (s *Service) List(ctx context.Context, owner string) ([]View, error) {
	cs, err := s.d.DB.ListConnections(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(cs))
	for _, c := range cs {
		out = append(out, view(c))
	}
	return out, nil
}

func (s *Service) get(ctx context.Context, owner, id string) (store.Connection, error) {
	c, err := s.d.DB.GetConnection(ctx, owner, id)
	if errors.Is(err, store.ErrNotFound) {
		return c, ErrNotFound
	}
	return c, err
}

// Get returns one of owner's connections, with where it is used.
func (s *Service) Get(ctx context.Context, owner, id string) (View, error) {
	c, err := s.get(ctx, owner, id)
	if err != nil {
		return View{}, err
	}
	uses, err := s.d.DB.ConnectionUses(ctx, id)
	if err != nil {
		return View{}, err
	}
	v := view(c)
	for _, u := range uses {
		v.UsedBy = append(v.UsedBy, Use{SpaceID: u.SpaceID, SpaceName: u.SpaceName, Environment: u.Environment, Name: u.Name})
	}
	return v, nil
}

// Update changes one of owner's connections. A new credential replaces the
// old one; a managed one that is no longer used is removed.
func (s *Service) Update(ctx context.Context, owner, id string, p Patch) (View, error) {
	c, err := s.get(ctx, owner, id)
	if err != nil {
		return View{}, err
	}
	name, settings := c.Name, map[string]any{}
	_ = json.Unmarshal([]byte(c.Settings), &settings)
	if p.Name != nil {
		name = *p.Name
	}
	if p.Settings != nil {
		settings = p.Settings
	}
	norm, err := s.check(name, c.Type, settings)
	if err != nil {
		return View{}, err
	}
	b, _ := json.Marshal(norm)
	old := c
	c.Name, c.Settings, c.UpdatedAt = name, string(b), s.d.Now()
	if p.Secret != nil {
		// A new managed value overwrites the same place (the reference is the
		// connection's), so the old one is removed only when the mode changes.
		mode, provider, ref, err := s.putSecret(ctx, id, c.Type, norm, *p.Secret)
		if err != nil {
			return View{}, err
		}
		c.SecretMode, c.SecretProvider, c.SecretRef = mode, provider, ref
	}
	// Settings changed without a new secret (an HTTP connection's auth).
	switch needed, forbidden, is := secretRule(c.Type, norm); {
	case needed && c.SecretMode == store.SecretNone:
		return View{}, invalid("%s connections with these settings need a secret: %s", c.Type, is)
	case forbidden && c.SecretMode != store.SecretNone:
		return View{}, invalid("%s connections with auth %s take no secret: send \"secret\": {\"none\": true} to remove it", c.Type, str(norm, "auth"))
	}
	if err := s.d.DB.UpdateConnection(ctx, c); err != nil {
		if p.Secret != nil && c.SecretMode == store.SecretManaged && old.SecretMode != store.SecretManaged {
			s.dropSecret(ctx, c.SecretMode, c.SecretRef)
		}
		if errors.Is(err, store.ErrNotFound) {
			return View{}, ErrNotFound
		}
		return View{}, err
	}
	if p.Secret != nil && old.SecretMode == store.SecretManaged && c.SecretMode != store.SecretManaged {
		s.dropSecret(ctx, old.SecretMode, old.SecretRef)
	}
	return view(c), nil
}

// Delete removes one of owner's connections, and its managed credential;
// ErrInUse while a space binds it.
func (s *Service) Delete(ctx context.Context, owner, id string) error {
	c, err := s.get(ctx, owner, id)
	if err != nil {
		return err
	}
	if err := s.d.DB.DeleteConnection(ctx, owner, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.dropSecret(ctx, c.SecretMode, c.SecretRef)
	return nil
}

// SpaceInput is a caller's bindings in a space: each environment's names
// -> one of the caller's connections (by name or ID). With Default set (an
// owner only), it also declares the space's environments and names from
// them, as before shared spaces: every environment must then bind the same
// names.
type SpaceInput struct {
	Default      string
	Environments map[string]map[string]string
}

// Bound is a name's connection, as callers see it.
type Bound struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// SpaceView is the space's environments with the caller's bindings, and
// the declared names each environment still needs from them.
type SpaceView struct {
	Default      string                      `json:"default_environment"`
	Environments map[string]map[string]Bound `json:"environments"`
	Missing      map[string][]string         `json:"missing,omitempty"`
}

// NamesInput is what a space's owners declare: environments, the default,
// and each name's connection type.
type NamesInput struct {
	Default      string
	Environments []string
	Names        map[string]string
}

// NamesView is a space's declarations, and which names each member has
// bound (principal -> environment -> names; never to which connection).
type NamesView struct {
	Default      string                         `json:"default_environment"`
	Environments []string                       `json:"environments"`
	Names        map[string]string              `json:"names"`
	Bound        map[string]map[string][]string `json:"bound"`
}

// ownSpace checks the caller's role in the space: ErrNotFound without one,
// a spaces.ForbiddenError if it is below need.
func (s *Service) ownSpace(ctx context.Context, caller, spaceID string, need spaces.Role) error {
	sp, err := s.d.DB.SpaceAccess(ctx, caller, spaceID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !spaces.Role(sp.Role).Allows(need) {
		return &spaces.ForbiddenError{Need: need}
	}
	return nil
}

// Names returns a space's declared names (any role).
func (s *Service) Names(ctx context.Context, caller, spaceID string) (NamesView, error) {
	if err := s.ownSpace(ctx, caller, spaceID, spaces.RoleRead); err != nil {
		return NamesView{}, err
	}
	sn, err := s.d.DB.GetSpaceNames(ctx, spaceID)
	if err != nil {
		return NamesView{}, err
	}
	bound, err := s.d.DB.BoundNames(ctx, spaceID)
	if err != nil {
		return NamesView{}, err
	}
	envs := sn.Environments
	if envs == nil {
		envs = []string{}
	}
	return NamesView{Default: sn.Default, Environments: envs, Names: sn.Names, Bound: bound}, nil
}

// SetNames replaces a space's declarations (owners). Members' bindings of
// an environment or name that goes, or of a name whose type changes, go.
func (s *Service) SetNames(ctx context.Context, caller, spaceID string, in NamesInput) (NamesView, error) {
	if err := s.ownSpace(ctx, caller, spaceID, spaces.RoleOwner); err != nil {
		return NamesView{}, err
	}
	sn, err := checkNames(in)
	if err != nil {
		return NamesView{}, err
	}
	if err := s.d.DB.SetSpaceNames(ctx, spaceID, sn); err != nil {
		return NamesView{}, err
	}
	return s.Names(ctx, caller, spaceID)
}

// checkNames checks declarations and settles the default.
func checkNames(in NamesInput) (store.SpaceNames, error) {
	if len(in.Environments) > MaxEnvironments {
		return store.SpaceNames{}, invalid("at most %d environments", MaxEnvironments)
	}
	if len(in.Names) > MaxNames {
		return store.SpaceNames{}, invalid("at most %d names", MaxNames)
	}
	envs := append([]string{}, in.Environments...)
	sort.Strings(envs)
	for i, env := range envs {
		if !identifier.MatchString(env) {
			return store.SpaceNames{}, invalid("environment names must start with a lowercase letter and have only a-z, 0-9 and _, got %q", env)
		}
		if i > 0 && envs[i-1] == env {
			return store.SpaceNames{}, invalid("environment %q is listed twice", env)
		}
	}
	for name, typ := range in.Names {
		if !identifier.MatchString(name) {
			return store.SpaceNames{}, invalid("names must start with a lowercase letter and have only a-z, 0-9 and _ (they are used in SQL, as in crm.orders), got %q", name)
		}
		if _, ok := types[typ]; !ok {
			return store.SpaceNames{}, invalid("%s: type must be one of %s, got %q", name, strings.Join(Types(), ", "), typ)
		}
	}
	sn := store.SpaceNames{Default: in.Default, Environments: envs, Names: in.Names}
	if sn.Names == nil {
		sn.Names = map[string]string{}
	}
	switch {
	case len(envs) == 0:
		if in.Default != "" {
			return store.SpaceNames{}, invalid("default_environment %q: there are no environments", in.Default)
		}
		if len(sn.Names) > 0 {
			return store.SpaceNames{}, invalid("names need at least one environment")
		}
	case in.Default == "" && len(envs) == 1:
		sn.Default = envs[0]
	case !slices.Contains(envs, in.Default):
		return store.SpaceNames{}, invalid("default_environment must be one of the environments (%v), got %q", envs, in.Default)
	}
	return sn, nil
}

// Space returns the space's environments with the caller's bindings, and
// what each environment still needs from them (any role).
func (s *Service) Space(ctx context.Context, owner, spaceID string) (SpaceView, error) {
	if err := s.ownSpace(ctx, owner, spaceID, spaces.RoleRead); err != nil {
		return SpaceView{}, err
	}
	sn, err := s.d.DB.GetSpaceNames(ctx, spaceID)
	if err != nil {
		return SpaceView{}, err
	}
	b, err := s.d.DB.GetBindings(ctx, spaceID, owner)
	if err != nil {
		return SpaceView{}, err
	}
	conns, err := s.d.DB.ListConnections(ctx, owner)
	if err != nil {
		return SpaceView{}, err
	}
	byID := map[string]store.Connection{}
	for _, c := range conns {
		byID[c.ID] = c
	}
	out := SpaceView{Default: sn.Default, Environments: map[string]map[string]Bound{}}
	for _, env := range sn.Environments {
		out.Environments[env] = map[string]Bound{}
		for name, id := range b[env] {
			c := byID[id] // the caller's own
			out.Environments[env][name] = Bound{ID: id, Name: c.Name, Type: c.Type}
		}
		if miss := missing(sn.Names, b[env], nil); len(miss) > 0 {
			if out.Missing == nil {
				out.Missing = map[string][]string{}
			}
			out.Missing[env] = miss
		}
	}
	return out, nil
}

// missing are the names of declared (only those in only, if not nil) that
// bound lacks, sorted.
func missing(declared map[string]string, bound map[string]string, only []string) []string {
	var out []string
	for name := range declared {
		if only != nil && !slices.Contains(only, name) {
			continue
		}
		if bound[name] == "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// SetSpace replaces the caller's bindings in the space (any role): each to
// one of their own connections, of the declared type, in a declared
// environment; names may stay unbound (runs then say which to bind). With
// in.Default set, or while the space declares nothing yet, an owner also
// declares the space's environments and names from the bindings (every
// environment must bind the same names), as before shared spaces.
func (s *Service) SetSpace(ctx context.Context, owner, spaceID string, in SpaceInput) (SpaceView, error) {
	if err := s.ownSpace(ctx, owner, spaceID, spaces.RoleRead); err != nil {
		return SpaceView{}, err
	}
	current, err := s.d.DB.GetSpaceNames(ctx, spaceID)
	if err != nil {
		return SpaceView{}, err
	}
	// Declaring: default_environment given, or nothing declared yet.
	declaring := in.Default != "" || len(current.Environments) == 0
	if declaring {
		if err := s.ownSpace(ctx, owner, spaceID, spaces.RoleOwner); err != nil {
			var f *spaces.ForbiddenError
			if errors.As(err, &f) && in.Default == "" {
				return SpaceView{}, invalid("this space declares no names yet: an owner declares them first")
			}
			return SpaceView{}, err
		}
	}
	if len(in.Environments) > MaxEnvironments {
		return SpaceView{}, invalid("at most %d environments", MaxEnvironments)
	}
	conns, err := s.d.DB.ListConnections(ctx, owner)
	if err != nil {
		return SpaceView{}, err
	}
	byKey := map[string]store.Connection{} // a connection's ID or name
	for _, c := range conns {
		byKey[c.ID], byKey[c.Name] = c, c
	}
	envs := make([]string, 0, len(in.Environments))
	for env := range in.Environments {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	b := store.Bindings{}
	typeOf := map[string]string{}
	var first []string // the first environment's names, which every other must match when declaring
	for i, env := range envs {
		if !identifier.MatchString(env) {
			return SpaceView{}, invalid("environment names must start with a lowercase letter and have only a-z, 0-9 and _, got %q", env)
		}
		binds := in.Environments[env]
		if len(binds) > MaxNames {
			return SpaceView{}, invalid("at most %d names per environment", MaxNames)
		}
		these := make([]string, 0, len(binds))
		b[env] = map[string]string{}
		for name, key := range binds {
			if !identifier.MatchString(name) {
				return SpaceView{}, invalid("names must start with a lowercase letter and have only a-z, 0-9 and _ (they are used in SQL, as in crm.orders), got %q", name)
			}
			c, ok := byKey[key]
			if !ok {
				return SpaceView{}, invalid("environment %s: %s: you have no connection %q", env, name, key)
			}
			b[env][name] = c.ID
			these = append(these, name)
			if t, seen := typeOf[name]; declaring && seen && t != c.Type {
				return SpaceView{}, invalid("%s is bound to a %s connection in one environment and a %s one in another", name, t, c.Type)
			}
			typeOf[name] = c.Type
		}
		sort.Strings(these)
		if !declaring {
			continue
		}
		if i == 0 {
			first = these
		} else if fmt.Sprint(these) != fmt.Sprint(first) {
			return SpaceView{}, invalid("every environment must bind the same names: %s binds %v, %s binds %v", envs[0], first, env, these)
		}
	}
	if declaring {
		sn, err := checkNames(NamesInput{Default: in.Default, Environments: envs, Names: typeOf})
		if err != nil {
			return SpaceView{}, err
		}
		if err := s.d.DB.SetSpaceNames(ctx, spaceID, sn); err != nil {
			return SpaceView{}, err
		}
	} else {
		sn := current
		for _, env := range envs {
			if !slices.Contains(sn.Environments, env) {
				return SpaceView{}, invalid("this space has no environment %q (its environments: %v)", env, sn.Environments)
			}
			for name, id := range b[env] {
				want, ok := sn.Names[name]
				switch {
				case !ok:
					return SpaceView{}, invalid("this space declares no name %q (ask an owner to add it)", name)
				case byKey[id].Type != want:
					return SpaceView{}, invalid("%s is a %s name: %q is a %s connection", name, want, byKey[id].Name, byKey[id].Type)
				}
			}
		}
	}
	if err := s.d.DB.SetBindings(ctx, spaceID, owner, b); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SpaceView{}, invalid("a connection, environment or name was deleted meanwhile: try again")
		}
		return SpaceView{}, err
	}
	return s.Space(ctx, owner, spaceID)
}
