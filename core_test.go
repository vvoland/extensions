// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package extensions

import (
	"context"
	"errors"
	"testing"

	"gotest.tools/v3/assert"
)

type recordingRegistrar struct {
	extensions []Extension
	err        error
}

func (r *recordingRegistrar) Register(ext Extension) error {
	if r.err != nil {
		return r.err
	}
	r.extensions = append(r.extensions, ext)
	return nil
}

func TestRegisterAllRegistersExtensions(t *testing.T) {
	registrar := &recordingRegistrar{}

	err := RegisterAll(registrar, New(Declaration{ID: "first"}), New(Declaration{ID: "second"}))
	assert.NilError(t, err)
	assert.Equal(t, len(registrar.extensions), 2)
	assert.Equal(t, registrar.extensions[0].Declaration().ID, ExtensionID("first"))
	assert.Equal(t, registrar.extensions[1].Declaration().ID, ExtensionID("second"))
}

func TestRegisterAllReturnsRegisterError(t *testing.T) {
	wantErr := errors.New("register failed")
	registrar := &recordingRegistrar{err: wantErr}

	err := RegisterAll(registrar, New(Declaration{ID: "extension"}))
	assert.ErrorIs(t, err, wantErr)
}

func TestDefinePointValidatesID(t *testing.T) {
	valid := []PointID{
		"org.mobyproject.extension.volume.driver.v1",
		"org.mobyproject.extension.container.create_hook.v0",
		"com.docker.compose.api.v12",
		"a.b.v0",
	}
	for _, id := range valid {
		t.Run("valid/"+string(id), func(t *testing.T) {
			assert.Equal(t, DefinePoint[any](id).ID(), id)
		})
	}

	invalid := []PointID{
		"",
		"org.mobyproject.greeter",            // no version
		"greeter.v1",                         // only one segment before the version
		"org.mobyproject.extension.v1.thing", // version not last
		"Org.Mobyproject.Greeter.v1",         // uppercase
		"org.mobyproject.greeter.v",          // no version number
	}
	for _, id := range invalid {
		t.Run("invalid/"+string(id), func(t *testing.T) {
			assert.Assert(t, panics(func() { DefinePoint[any](id) }), "DefinePoint(%q) should panic", id)
		})
	}
}

func panics(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
}

func TestValidateExtensionID(t *testing.T) {
	valid := []ExtensionID{
		"org.example.no-privileged.v1",
		"com.docker.compose.v1",
		"com.docker.mobyextension.nri.v1",
		"org.example.s3-volume.v2",
		"org.mobyproject.example.greeter.v0",
	}
	for _, id := range valid {
		if err := ValidateExtensionID(id); err != nil {
			t.Errorf("ValidateExtensionID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []ExtensionID{
		"",
		"single",                     // not reverse-DNS (one segment)
		"org.example.no-privileged",  // missing version segment
		"com.docker.compose",         // missing version segment
		"foo.v1",                     // version but only one name segment
		"Org.Example.Ext.v1",         // uppercase
		"org.example/evil.v1",        // path separator
		"org.example.../etc",         // path traversal shape
		"org.example.-bad.v1",        // segment leads with hyphen
		"org.example.bad-.v1",        // segment trails with hyphen
		"org.example.a b.v1",         // whitespace
		"org..example.v1",            // empty segment
		"org.example.under_score.v1", // underscore not allowed in extension ids
	}
	for _, id := range invalid {
		if err := ValidateExtensionID(id); err == nil {
			t.Errorf("ValidateExtensionID(%q) = nil, want error", id)
		}
	}
}

func TestValidateExtensionConfigKey(t *testing.T) {
	valid := []ExtensionID{
		"org.example.myext.v1",
		"org.example.myext.v*",
		"org.example.s3-volume.v*",
		"org.example.v*",
	}
	for _, key := range valid {
		if err := ValidateExtensionConfigKey(key); err != nil {
			t.Errorf("ValidateExtensionConfigKey(%q) = %v, want nil", key, err)
		}
	}

	invalid := []ExtensionID{
		"",
		".v*",                   // no name
		"foo.v*",                // only one name segment
		"org.example.myext",     // missing version
		"org.example.myext.*",   // wildcard must be ".v*"
		"org.example.myext.v",   // missing version number
		"org.example.myext.v1*", // wildcard after a version
		"org.example.*.v1",      // wildcard in a name segment
		"Org.Example.myext.v*",  // uppercase
		"org.example/evil.v*",   // path separator
	}
	for _, key := range invalid {
		if err := ValidateExtensionConfigKey(key); err == nil {
			t.Errorf("ValidateExtensionConfigKey(%q) = nil, want error", key)
		}
	}
}

func TestLookupConfig(t *testing.T) {
	exact := Config{"source": "exact"}
	anyVersion := Config{"source": "any version"}
	configs := map[ExtensionID]Config{
		"org.example.foo.v1": exact,
		"org.example.foo.v*": anyVersion,
		"org.example.bar.v1": {"source": "bar"},
	}

	for _, tc := range []struct {
		name string
		id   ExtensionID
		want Config
	}{
		{name: "exact id takes precedence", id: "org.example.foo.v1", want: exact},
		{name: "any-version key covers other versions", id: "org.example.foo.v2", want: anyVersion},
		{name: "other extension without a key", id: "org.example.baz.v1", want: nil},
		{name: "exact key does not cover other versions", id: "org.example.bar.v2", want: nil},
		{name: "name prefix does not match", id: "org.example.foobar.v1", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.DeepEqual(t, LookupConfig(configs, tc.id), tc.want)
		})
	}
}

func TestValidateExtensionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity ExtensionIdentity
		wantErr  string
	}{
		{
			name: "valid builtin",
			identity: ExtensionIdentity{
				ID:     "org.example.valid.v1",
				Origin: ExtensionOrigin{Kind: ExtensionOriginBuiltin},
			},
		},
		{
			name: "valid executable",
			identity: ExtensionIdentity{
				ID: "org.example.valid.v1",
				Origin: ExtensionOrigin{
					Kind:       ExtensionOriginExecutable,
					Executable: &ExecutableOrigin{Path: "/test/bin/org.example.valid.v1"},
				},
			},
		},
		{
			name:     "missing kind",
			identity: ExtensionIdentity{ID: "org.example.valid.v1"},
			wantErr:  "extension origin kind is required",
		},
		{
			name: "unknown kind",
			identity: ExtensionIdentity{
				ID:     "org.example.valid.v1",
				Origin: ExtensionOrigin{Kind: "remote"},
			},
			wantErr: `invalid extension origin kind "remote"`,
		},
		{
			name: "builtin with executable payload",
			identity: ExtensionIdentity{
				ID: "org.example.valid.v1",
				Origin: ExtensionOrigin{
					Kind:       ExtensionOriginBuiltin,
					Executable: &ExecutableOrigin{Path: "/test/bin/org.example.valid.v1"},
				},
			},
			wantErr: "builtin extension origin must not include executable payload",
		},
		{
			name: "executable missing payload",
			identity: ExtensionIdentity{
				ID:     "org.example.valid.v1",
				Origin: ExtensionOrigin{Kind: ExtensionOriginExecutable},
			},
			wantErr: "executable extension origin requires executable payload",
		},
		{
			name: "executable empty path",
			identity: ExtensionIdentity{
				ID: "org.example.valid.v1",
				Origin: ExtensionOrigin{
					Kind:       ExtensionOriginExecutable,
					Executable: &ExecutableOrigin{},
				},
			},
			wantErr: "executable origin path is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExtensionIdentity(tc.identity)
			if tc.wantErr == "" {
				assert.NilError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestSingleSelection(t *testing.T) {
	stock := callerFunc(func(context.Context) error { return nil })
	custom := callerFunc(func(context.Context) error { return nil })

	t.Run("builtin alone stands in", func(t *testing.T) {
		got, err := testPoint.Single(resolverOf(
			resolvedProvider("org.mobyproject.stock.v1", ExtensionOriginBuiltin, stock),
		))
		assert.NilError(t, err)
		assert.NilError(t, got.Call(t.Context()), "the default must stand in when nothing is installed")
	})

	t.Run("executable provider masks the builtin", func(t *testing.T) {
		called := ""
		stockC := callerFunc(func(context.Context) error { called = "stock"; return nil })
		customC := callerFunc(func(context.Context) error { called = "custom"; return nil })
		got, err := testPoint.Single(resolverOf(
			resolvedProvider("org.mobyproject.stock.v1", ExtensionOriginBuiltin, stockC),
			resolvedProvider("org.example.custom.v1", ExtensionOriginExecutable, customC),
		))
		assert.NilError(t, err)
		assert.NilError(t, got.Call(t.Context()))
		assert.Equal(t, called, "custom", "the executable provider must replace the builtin, not conflict with it")
	})

	t.Run("two executable providers are rejected", func(t *testing.T) {
		_, err := testPoint.Single(resolverOf(
			resolvedProvider("org.mobyproject.stock.v1", ExtensionOriginBuiltin, stock),
			resolvedProvider("org.example.one.v1", ExtensionOriginExecutable, custom),
			resolvedProvider("org.example.two.v1", ExtensionOriginExecutable, custom),
		))
		assert.ErrorContains(t, err, "multiple providers")
	})

	t.Run("no providers is an error", func(t *testing.T) {
		_, err := testPoint.Single(resolverOf())
		assert.ErrorContains(t, err, "no providers")
	})

	t.Run("wrong provider type names the extension", func(t *testing.T) {
		_, err := testPoint.Single(resolverOf(
			resolvedProvider("org.example.broken.v1", ExtensionOriginExecutable, 42),
		))
		assert.ErrorContains(t, err, `extension "org.example.broken.v1"`)
	})
}

func TestEffectiveProviders(t *testing.T) {
	ids := func(providers []ResolvedProvider) []ExtensionID {
		out := make([]ExtensionID, 0, len(providers))
		for _, p := range providers {
			out = append(out, p.Identity.ID)
		}
		return out
	}
	def := resolvedProvider("d", ExtensionOriginBuiltin, nil)
	a := resolvedProvider("a", ExtensionOriginExecutable, nil)
	b := resolvedProvider("b", ExtensionOriginExecutable, nil)

	assert.Equal(t, len(EffectiveProviders(nil)), 0)
	assert.DeepEqual(t, ids(EffectiveProviders([]ResolvedProvider{def})), []ExtensionID{"d"})
	assert.DeepEqual(t, ids(EffectiveProviders([]ResolvedProvider{def, a})), []ExtensionID{"a"})
	assert.DeepEqual(t, ids(EffectiveProviders([]ResolvedProvider{def, a, b})), []ExtensionID{"a", "b"})
}

type stubResolver struct{ providers []ResolvedProvider }

func (stubResolver) Provider(PointID, ExtensionID) (any, error) { return nil, nil }
func (r stubResolver) Providers(PointID) []ResolvedProvider     { return r.providers }

type caller interface {
	Call(ctx context.Context) error
}

type callerFunc func(ctx context.Context) error

func (f callerFunc) Call(ctx context.Context) error { return f(ctx) }

var testPoint = DefinePoint[caller]("org.example.fanout.test.v0")

func resolverOf(providers ...ResolvedProvider) Resolver { return stubResolver{providers: providers} }

func resolvedProvider(id ExtensionID, origin ExtensionOriginKind, impl any) ResolvedProvider {
	identity := ExtensionIdentity{ID: id, Origin: ExtensionOrigin{Kind: origin}}
	if origin == ExtensionOriginExecutable {
		identity.Origin.Executable = &ExecutableOrigin{Path: "test-executable"}
	}
	return ResolvedProvider{Identity: identity, Impl: impl}
}
