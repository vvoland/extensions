# Authoring extension points and extensions

This is the procedural guide.
Read [DESIGN.md](./DESIGN.md) for current rules and the [overview](../README.md#how-it-works) for the main concepts.
This guide describes the standalone Go module; snippets with placeholder package names illustrate how to embed it in your application.
The repository does not provide Docker daemon configuration, CLI flags, or engine hook points.

## Point authoring

### Files and source of truth

Create `extpoints/<area>/<name>/v0/` in your own Go module.
This layout is a convention, not a required location.
A point has one hand-written Go contract and generated wire files:

```
extpoints/<area>/<name>/v0/
  <name>.go                 # package doc, //go:generate, contract, helpers
  <service>.proto            # generated from the Go contract
  protogen/                  # generated; do not edit
    <service>.pb.go          # generated proto messages
    wire.gen.go              # service, client, ClientPoint, ServerPoint,
                             # adapters, and conversions
```

The Go file is the source of truth.
Do not hand-edit the `.proto` file or anything in `protogen/`; `mobyextgen` regenerates them.

### 1. Write the Go contract

Create `extpoints/<area>/<name>/v0/<name>.go`.
New points start at `v0`, which is experimental and may change without compatibility until promoted to `v1`.
The contract contains the provider interface, message structs, `Point` value, and helpers the engine calls.

This small service point shows the required shape:

```go
// Package greeterv0 is the greeter extension point contract.
package greeterv0

import (
	"context"

	"github.com/moby/extensions"
)

// Greeter is the provider interface.
type Greeter interface {
	Greet(ctx context.Context, req *HelloRequest) (*HelloReply, error)
}

// HelloRequest is the request message.
type HelloRequest struct {
	Name string `pb:"1"`
}

// HelloReply is the response message.
type HelloReply struct {
	Message string `pb:"1"`
}

// Point binds the interface to a namespaced, versioned id. Mobyextgen infers
// the gRPC service as <PointID>.<InterfaceName>.
var Point = extensions.DefinePoint[Greeter]("org.mobyproject.extension.example.greeter.v0")

// Greet resolves the provider and calls it.
func Greet(ctx context.Context, resolver extensions.Resolver, req *HelloRequest) (*HelloReply, error) {
	g, err := Point.Single(resolver)
	if err != nil {
		return nil, err
	}
	return g.Greet(ctx, req)
}
```

The interface name is the gRPC service-name component. Keep it stable within a
Point version. If a clearer Go name is useful, add an alias without changing the
type used by `DefinePoint`:

```go
type LegacyGreeter interface {
	Greet(context.Context, *HelloRequest) (*HelloReply, error)
}

type Greeter = LegacyGreeter

var Point = extensions.DefinePoint[LegacyGreeter]("org.example.greeter.v0")
```

Changing the wire service name requires a new Point version.

Before coding, record the id and version, resolution shape and cardinality, ordering, call/veto semantics, dependencies, and failure policy.
The detailed rules are [DESIGN.md](./DESIGN.md#resolution-ordering-and-lifecycle); the authoring-specific choices are:

- Use `org.mobyproject.extension.<area>.<name>.v0` for an engine point or a vendor reverse-DNS namespace, and a new `.vN` for a breaking change.
- Call `Point.Single`, `Point.All`, or `Point.ByExtension` from a helper rather than from engine code.
  Use `DefineSinglePoint` for a deciding point; the generated `ClientPoint` makes the host reject two executable providers.
  Built-ins yield to executable providers.
  Runtime fallback and failure behavior belongs to the point's call helper and must be stated in its contract.
  Omit a built-in point's `ClientPoint` from `host.WithClientProviders` to close it to internal replacement; a launched declaration requiring that client wiring is then rejected.
- Prefer unary `M(ctx, *Req) (*Resp, error)` methods and explicit phases such as `Update` followed by `Validate`; keep dependencies acyclic and optional when absence is valid.
- Choose fail-open or fail-closed behavior.
  Security and veto points normally fail closed.
  Use `extensions.Policy` with `Each` or `Fold`:

  ```go
  var policy = extensions.Policy{Timeout: 30 * time.Second}

  func Validate(ctx context.Context, resolver extensions.Resolver, req *Request) error {
      vetoing := policy
      vetoing.Action = "vetoed the start"
      return extensions.Each(ctx, Point, resolver, vetoing,
          func(ctx context.Context, p Provider) error { return p.Validate(ctx, req) })
  }
  ```

  `Policy.Timeout` gives each provider a fresh deadline and `FailOpen` skips a failure.
  Errors identify their extension.
  Each call's gRPC deadline is enforced out of process; an in-process provider only receives a context and must honor it, because abandoning a direct call could leave it mutating shared state.
  Do not claim this timeout protects against a hanging in-process provider.
- Use plain `pb:"N"`-tagged structs, stable field numbers, and existing data formats directly.

Supported fields are scalar values, `string`, `[]byte`, repeated scalars such as `[]string`, string-keyed scalar maps, pointer nested messages such as `*Other`, and repeated nested messages such as `[]Other`.
A single nested message must be a pointer; the generator rejects a value field.

The current framework is experimental and does not preserve compatibility for
removed fields. For proto3 presence, unsupported types, and wire-shape rules,
see [DESIGN.md#wire-contract](./DESIGN.md#wire-contract).

Points the engine offers to extensions use the same interface shape; only caller direction changes.

### 2. Add generation

Put the directive at the top of the contract file.
It is identical for every point; there are no paths to adjust:

```go
//go:generate go run github.com/moby/extensions/cmd/mobyextgen

// Package <name>v0 is the <name> extension point contract, written Go-first ...
package <name>v0
```

`mobyextgen` reads the Go contract at generation time and writes the `.proto`,
protobuf message code, and `wire.gen.go`.
For an ordinary Point, it infers the gRPC service name as `<PointID>.<InterfaceName>`.
The generated package exports:

- `ServerPoint`, for serving the point to an SDK server or a dependency callback;
- `ClientPoint`, for host wiring to a launched provider;
- `NewClient(conn)`, which returns the handwritten Go interface over a gRPC connection.

The contract package does not import protobuf packages; generated code belongs in `protogen/`.
The generator derives the import path from `go.mod` and the proto file name
from the inferred service identity.

### 3. Generate and validate

Generation needs Go 1.26 or newer, but no `protoc`, plugins, or other `PATH` tools.
In your own module, record the generator as a tool dependency before running the directive:

```fish
go get -tool github.com/moby/extensions/cmd/mobyextgen
go generate ./extpoints/<area>/<name>/v0/
go mod tidy
go test ./...
```

Replace `<area>` and `<name>` with your package directory names.
The directive uses the generator version selected by your module's dependency graph.
The tool dependency keeps the generator's dependencies and checksums available after `go mod tidy`.
Commit the generated output along with `go.mod` and `go.sum`.

When contributing to this repository, regenerate all contracts from the repository root:

```fish
go generate ./...
```

Check that committed output matches fresh generation without rewriting the checkout:

```fish
make lint-generate
```

CI runs this check as part of `make lint`, using the Go version from `go.mod` in a container.
The direct `make lint-generate` command uses your local Go toolchain.

### 4. Call the point from an engine flow

Import the contract and call its helper with the host as `extensions.Resolver`.
The `*host.Host` returned by `host.New` satisfies that interface.
`Point.All` returns an empty list when there are no providers; `Point.Single` returns an error.
The helper must define whether absence is a safe no-op or a failure.
Call the helper at the engine boundary where its input is complete.
A security policy point must inspect the authoritative data used by the protected operation so an earlier or partial representation cannot bypass it.

### 5. Support separate-binary providers

Steps 1–4 are sufficient for in-process providers.
To support a launched provider, pass its generated `ClientPoint` when constructing the Host:

```go
host.WithClientProviders(mypointpb.ClientPoint),
```

`ClientPoint` builds a typed in-process caller from the gRPC connection, so daemon code calls a launched provider like a built-in one.
Wire it only for Points the daemon itself calls.
This list is the wiring boundary for internal calls to launched providers: an unlisted declared point is rejected unless it is offered only for external publication.
Use `host.WithProviderPolicy` to restrict which extensions may provide internally wired points.
See [DESIGN.md#discovery-security](./DESIGN.md#discovery-security).

A Point that only external callers use does not need `ClientPoint` wiring.
Offer it for publication instead: the proxy forwards calls to the extension by gRPC service name without decoding them.
For example, a naming Point that the daemon calls needs `ClientPoint`, while a job API that only external tools call needs only an offer.

### Publishing an ordinary Point

Publication uses the same ordinary Point contract; there is no standalone
service contract. Implement the Point and separately offer it:

```go
var Extension = extensions.New(extensions.Declaration{
	ID: "org.example.greeter.v1",
	Providers: []extensions.Provider{
		greeterv0.Point.Provide(greeter{}),
		servicev0.Offer(greeterv0.Point),
	},
})
```

One offer can select a subset of several implemented Points:

```go
Providers: []extensions.Provider{
	foov1.Point.Provide(fooImpl{}),
	barv1.Point.Provide(barImpl{}),
	servicev0.Offer(foov1.Point),
}
```

The Host drops publication by default. Allow an offer only through explicit
policy:

```go
_, err := host.New(ctx, host.WithProviderPolicy(host.PointPolicyFunc(func(identity extensions.ExtensionIdentity, point extensions.PointID) host.PointPolicyResult {
	if identity.ID == "org.example.greeter.v1" &&
		(point == greeterv0.Point.ID() || point == servicev0.Point.ID()) {
		return host.Allow()
	}
	return host.Drop()
})))
```

Publication authorization uses `servicev0.Point.ID()` and receives the host-attested identity.
Internal provider admission uses the same policy with ordinary Point IDs.
A nil policy allows every internally wired provider and drops all publication.
A non-nil policy decides each use by identity and Point:

- `Allow` admits the provider or publication.
- `Drop` omits an ordinary provider and its offer.
- `Reject` fails Host construction and preserves its error cause.

A zero result and a typed nil `PointPolicyFunc` reject the request.
The `servicev0` offer marker itself is exempt from provider admission.
Offered-only process Points have no internal provider, so the policy sees them only through `servicev0.Point.ID()`.

If policy drops every ordinary provider of an extension, the Host skips the extension instead of initializing it.
A launched extension stays loaded while it publishes an offered-only Point, because external callers still reach that Point through the proxy.
For example, when policy drops the naming Point an extension provides to the daemon but allows publication, the extension keeps serving its offered-only job API.

For an in-process extension, supply generated adapters at Host composition:

```go
host.WithPointServers(greeterpb.ServerPoint),
```

A separate binary passes every ordinary provider's generated `ServerPoint` to
the SDK, whether the Point is internal, offered, or both:

```go
sdk.Main(extension, sdk.WithServerPoints(greeterpb.ServerPoint))
```

External callers use the generated handwritten client over the host connection:

```go
client := greeterpb.NewClient(hostConn)
reply, err := client.Greet(ctx, &greeterv0.HelloRequest{Name: "world"})
```

The current backend is gRPC. For a launched extension, the SDK registers the
generated service, the daemon records its fully qualified name, and the proxy
forwards the raw gRPC stream, including metadata and status. Arbitrary raw gRPC
publication is not part of the extension API.

## Writing an extension

An extension is an `extensions.Extension` value; constructing and passing it to a host or SDK server activates it.
There is no global registry or `func init()`.

### Stateless extension

Wrap a declaration with `extensions.New`:

```go
var Extension = extensions.New(extensions.Declaration{
	ID:        "org.example.myext.v1",
	Providers: []extensions.Provider{mypointv0.Point.Provide(&provider{})},
})
```

`Point.Provide(impl)` ties an implementation to a point; the implementation only needs to satisfy that point's Go interface.

### Stateful extension

Implement `extensions.Extension` when the extension owns state.
The same object can configure itself, provide points, and shut down:

```go
var Extension extensions.Extension = &Bridge{}

func (b *Bridge) Declaration() extensions.Declaration {
	return extensions.Declaration{
		ID:        ExtensionID,
		Providers: []extensions.Provider{mypointv0.Point.Provide(b)},
		Init:      b.init,
		Shutdown:  b.Stop,
	}
}
```

`Init` receives the extension config and a resolver.
The application supplies config keyed by id, or by a `.v*` key for every version, through `host.WithExtensionConfig`; declare dependencies and conflicts in the declaration.
The broker initializes dependencies before dependents.

### Retain dependencies for later calls

`Init` is where the host supplies the resolver.
If an extension needs a dependency after initialization, resolve it in `Init` and
store the typed interface on the same stateful extension object:

```go
type Consumer struct {
	greeter greeterv0.Greeter
}

func (c *Consumer) init(
	_ context.Context,
	_ extensions.Config,
	resolver extensions.Resolver,
) error {
	greeter, err := greeterv0.Point.Single(resolver)
	if err != nil {
		return err
	}
	c.greeter = greeter
	return nil
}

func (c *Consumer) Handle(
	ctx context.Context,
	req *greeterv0.HelloRequest,
) (*greeterv0.HelloReply, error) {
	return c.greeter.Greet(ctx, req)
}
```

The dependency declaration controls presence and initialization order; it does
not inject a field automatically.
Resolve during `Init` so a missing or ambiguous required dependency fails before
the extension starts serving.
Store the resolver itself only when lookup must be deferred.

For a fan-out point, store the typed provider list returned by `All`:

```go
type Consumer struct {
	greeters []extensions.TypedProvider[greeterv0.Greeter]
}

func (c *Consumer) init(
	_ context.Context,
	_ extensions.Config,
	resolver extensions.Resolver,
) error {
	var err error
	c.greeters, err = greeterv0.Point.All(resolver)
	return err
}
```

Use `ByExtension` when the extension must call one named provider.
For an optional dependency, declare `Point.OptionalDependency()`, check
`Point.Enabled(resolver)` in `Init`, and leave the stored provider empty when the
point is absent.

The same pattern works out of process: the resolved interface may be a generated
gRPC client and can be stored for normal operation.
The binary must still call `Server.Depends` for the point, and the host must
offer its generated `ServerPoint` as described below.
The current SDK closes the dependency callback connection before it calls the
extension's `Shutdown` hook, so a retained out-of-process dependency must not be
used from `Shutdown`.

For an out-of-process extension, wire every dependency point it will call:

```go
srv := sdk.NewServer()
if err := srv.Register(ext, mypointpb.ServerPoint); err != nil {
	return err
}
srv.Depends(volumedriverpb.ClientPoint) // one per dependency point it will call
return srv.Listen(ctx)
```

The Host must offer the same points as dependencies by passing their generated `ServerPoint` to `host.WithDependencyProviders`.
A dependency on an extension-defined point works only when the application supplies that wiring.
Cross-process dependency lookup currently exposes one provider; `All` and by-id selection are deferred.

Configure extensions when constructing the Host:

```go
host.WithExtensionConfig(map[extensions.ExtensionID]extensions.Config{
	"com.example.myext.v1": {"some_key": "value"},
	"com.example.other.v*": {"some_key": "value"},
}),
```

The same entry reaches an in-process `Init` or a separate binary through the startup handshake.
A key with the version replaced by `.v*`, such as `com.example.other.v*`, applies to every version of that extension.
A key with the full id, such as `com.example.other.v2`, takes precedence over it.
Because each version is a separate extension, a `.v*` entry must suit the configuration format of every version it reaches.
The application chooses how to read and validate this configuration; the framework does not read `daemon.json`.
The Host does not validate keys and silently ignores one that matches no extension; check their format with `extensions.ValidateExtensionConfigKey`.

### Run in-process

Register in-process extensions with `host.WithExtensions`:

```go
h, err := host.New(ctx,
	host.WithExtensions(myext.Extension),
	host.WithExtensionConfig(extensionConfig),
)
if err != nil {
	return err
}
```

Built-ins use the same registration path as launched binaries.
Keep `h` for point resolution and call `h.Shutdown` when the application exits, using a context that has not already been canceled.

### Run out-of-process

Write a `main` that builds an SDK server, registers the extension, and passes a
generated `ServerPoint` for each Point through `WithServerPoints`:

```go
func main() {
	sdk.Main(myext.Extension, sdk.WithServerPoints(mypointpb.ServerPoint))
}
```

Use `sdk.NewServer` instead when the binary needs `Depends` or other server setup.
`sdk.Main` handles daemon stop signals, serves the extension, and exits non-zero on failure.
The SDK runs the same `Init` and `Shutdown` lifecycle as an in-process host; only packaging differs.

`stdout` is reserved for the runtime handshake.
Log to `stderr`.
The daemon writes startup config to stdin and waits for one `ready\n` line on stdout; the SDK writes it once listening.
Any other pre-readiness stdout corrupts the handshake and fails launch.
The daemon captures stderr in its logs.

Deploy the binary with the extension id as its name; on Windows the name is `<id>.exe`.
The embedding application chooses the binary directories and runtime directory:

```go
h, err := host.New(ctx,
	host.WithDirs(extensionDir),
	host.WithRuntimeDir(runtimeDir),
	host.WithClientProviders(mypointpb.ClientPoint),
	host.WithExtensionConfig(extensionConfig),
)
if err != nil {
	return err
}
```

`host.New` discovers and launches accepted binaries in the supplied directories.
There is no default discovery directory or `--extension-dir` flag in this library.
Use trusted binary directories and a private runtime directory writable only by the Host's user, with a short absolute path for Unix sockets.
Create the private runtime directory before constructing the Host, and supply it whenever launching processes or exposing dependency callbacks.
Keep the construction context alive while launched processes are in use, and call `h.Shutdown` before the application exits.
See [discovery security](./DESIGN.md#discovery-security) before installing binaries.

There is no watchdog yet.
If the process dies, callers get gRPC errors until the daemon restarts.
See the [lifecycle limitations](./DESIGN.md#resolution-ordering-and-lifecycle).

## Author checklist

- [ ] Put the hand-written contract under `extpoints/<area>/<name>/v<N>/`.
- [ ] For an ordinary Point, use a valid point id and stable interface name and `pb` field numbers; let the generator infer the service name.
- [ ] Define resolution/cardinality, ordering, call/veto, dependency, and failure behavior in the point contract.
- [ ] Generate and commit `.proto` and `protogen/` output.
- [ ] Call a helper from the engine flow, passing the host resolver.
- [ ] Add generated `ClientPoint` wiring when the daemon calls a separate-binary provider internally.
- [ ] Add Host provider-admission policy when internal providers need an allowlist.
- [ ] Add dependency `ServerPoint` wiring when separate binaries call engine points.
- [ ] Register every ordinary provider's `ServerPoint` in a separate binary.
- [ ] Implement every published API as an ordinary `Point.Provide(impl)` provider.
- [ ] Offer selected implemented Points with `servicev0.Offer`.
- [ ] Add Host `WithProviderPolicy` and in-process `WithPointServers` wiring as needed.
- [ ] Keep handshake output on stdout and logs on stderr.
- [ ] Install one correctly named, non-world-writable binary in the trusted extension directory.

## Quick reference

- Define a Point with a Go interface, `pb` messages, `DefinePoint`, and call helpers in your contract package.
- Generate its `.proto` and `protogen/` files with `go generate`; the wire service name is `<PointID>.<InterfaceName>`.
- Implement an extension with `extensions.New(Declaration{…})` or an `Extension` implementation.
- Register it in-process with `host.WithExtensions`, or serve it in a binary with `sdk.Main` and `sdk.WithServerPoints`.
- Discover binaries with `host.WithDirs` and provide socket storage with `host.WithRuntimeDir`.
- Wire internal calls to binaries with `host.WithClientProviders`.
- Wire calls from binaries to Host providers with `host.WithDependencyProviders` and SDK `Server.Depends`.
- Supply configuration by extension id, or by a `.v*` key for every version, with `host.WithExtensionConfig`.
- Resolve Points through the Host and call their contract helpers.
- Restrict internal providers with `host.WithProviderPolicy` using ordinary Point IDs.
- Offer implemented Points with `servicev0.Offer`, authorize publication through policy on `servicev0.Point.ID()`, and supply `host.WithPointServers` for in-process offers.
- Call published Points with the generated `NewClient(hostConn)`.
- Release lifecycle resources with `Host.Shutdown`.
