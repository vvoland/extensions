# TODO: Split protobuf generation and bind Points without a server catalog

## Status

This document proposes a follow-up design. It does not describe current
behavior.

The current implementation requires generated `ServerPoint` values at process
and Host composition boundaries:

```go
sdk.Main(extension, sdk.WithServerPoints(greeterpb.ServerPoint))
```

```go
host.Options{
	PointServers: []serverpoint.Registration{
		greeterpb.ServerPoint,
	},
}
```

The target design removes those server-registration catalogs. It does so by
making an optional protobuf invocation binding part of the ordinary Point
definition and by making gRPC a generic consumer of that binding.

This is not only a package move. It changes how executable server wiring is
owned and discovered, so it must be implemented and reviewed separately from
the existing Host-owned publication work.

## Summary

Split generated code into three concerns:

```text
<point>/protogen/
    protobuf messages and descriptors only

<point>/
    handwritten Point interface and domain types
    generated protobuf binding and domain/protobuf conversions

<point>/grpcgen/
    typed gRPC clients and optional raw gRPC convenience APIs
```

A publishable Point carries generated protobuf metadata and invocation
functions. The binding is attached to the Point definition, not to an extension
declaration, publication offer, SDK entrypoint, Host option, or process-global
registry.

The generic Host/SDK gRPC bridge consumes that binding to construct a gRPC
service for a provider implementation. Therefore neither side needs a
per-Point generated `ServerPoint`:

```go
sdk.Main(extension)
```

`Main` retains its functional-option signature for SDK runtime configuration,
but Point server wiring is no longer an option.

```go
host.Options{
	Extensions:       installed,
	AllowPublication: publicationPolicy,
	ReservedServices: reservedServices,
}
```

The extension implementation and declaration remain transport-agnostic. They
provide and offer an ordinary Point:

```go
Providers: []extensions.Provider{
	greeterv0.Point.Provide(greeter{}),
	servicev0.Offer(greeterv0.Point),
}
```

The Point contract becomes protobuf-bound, but not gRPC-bound. A different RPC
transport that uses protobuf messages could consume the same binding.

## Problem

An in-process provider is only a Go implementation. To publish it on a gRPC
server, the Host currently also needs generated executable behavior:

- the protobuf service and message descriptors;
- request and response constructors;
- domain-to-protobuf and protobuf-to-domain conversions;
- method dispatch against the handwritten Go interface;
- a `grpc.ServiceDesc`; and
- gRPC handler registration.

`PointID` supplies identity only. Reflection over a Point implementation cannot
recover semantic conversions between handwritten domain structs and generated
protobuf messages.

The current solution passes a generated `ServerPoint` registration into each
composition root. This is explicit and safe, but it requires the embedding Host
to link a list of Point-specific adapters even when publication policy is the
only product-specific decision the Host should make.

Moving `ServerPoint` onto the extension is not acceptable. Extension
implementations and declarations must not select or configure a transport.
`servicev0.Offer` must also remain metadata-only, because an offer is eligibility
for Host-controlled publication rather than executable registration or
authorization.

## Design constraints

The split must preserve these invariants:

1. Extension implementation and declaration source does not import protobuf or
   gRPC packages.
2. `servicev0.Offer` contains Point metadata only.
3. Host policy remains the only publication authority and defaults to deny.
4. Point identity remains `<PointID>.<InterfaceName>` on the wire.
5. The interface name remains part of the versioned wire contract.
6. Importing a package does not register adapters in a process-global registry.
7. No `func init()` registry, `containerd/typeurl`, reflection-only dispatch, or
   dynamic Go plugin is used to discover executable code.
8. In-process calls continue to invoke the ordinary Go implementation directly.
9. Process publication continues to report and proxy fully qualified gRPC
   service names.
10. Reserved-service and extension-service collisions fail during startup.
11. Internal-only Points may remain free of protobuf bindings.
12. Generated output remains reproducible with only the Go toolchain.

The framework and runtime protocol are experimental, so this work may remove or
reshape generated APIs without compatibility shims.

## Important distinction: `protogen` versus `grpcgen`

A runtime Point value cannot cause Go to import a sibling package such as
`greeterv0/grpcgen`. If Point-specific server code remains exclusively in
`grpcgen`, the binary must still link it through one of:

- an explicit catalog;
- explicit per-Point registration;
- blank imports plus a global registry; or
- generated Host composition code.

The target design avoids the catalog by moving the type-specific server
invocation behavior into the Point's protobuf binding. The Host then uses one
generic gRPC adapter for all protobuf-bound Points.

Therefore the Host does not literally discover and call a per-Point `grpcgen`
package. Instead, the Point binding and `grpcgen` clients both derive from the
same protobuf contract:

```text
                         +--> generic Host/SDK gRPC server
Point protobuf binding -+
                         +--> generated grpcgen client
```

If `grpcgen` continues to contain a generated server API, that API is optional
convenience for direct users. Framework publication must not depend on it.

## Proposed package dependency graph

For a Greeter Point, dependencies should flow in one direction:

```text
greeterv0/protogen
    imports protobuf runtime only

point protobuf binding in greeterv0
    imports extensions binding API
    imports greeterv0/protogen

greeterv0/grpcgen
    imports greeterv0
    imports greeterv0/protogen
    imports gRPC

servicegrpc
    imports extensions binding API
    imports protobuf runtime
    imports gRPC
```

`protogen` must not import `greeterv0`. That breaks the current cycle and allows
a generated file in the handwritten Point package to import `protogen` and
implement domain/protobuf conversion there.

The handwritten Point package must not import `grpcgen` or gRPC. Consequently,
`grpcgen` may import the Point package, but the reverse dependency is forbidden.

## Protobuf binding model

### Required information

A protobuf binding for one Point service must expose enough information for a
generic RPC transport to decode, invoke, and encode each method. Conceptually:

```go
type ServiceBinding interface {
	Descriptor() protoreflect.ServiceDescriptor
	Methods() []MethodBinding
}

type MethodBinding struct {
	Descriptor protoreflect.MethodDescriptor
	NewRequest func() proto.Message
	Invoke     func(
		ctx context.Context,
		impl any,
		request proto.Message,
	) (proto.Message, error)
}
```

These are design sketches, not final API names.

For each method, generated `Invoke` code must:

1. assert the provider implementation is the Point's handwritten interface;
2. assert the protobuf request is the expected generated message type;
3. convert the protobuf request into the handwritten request type;
4. call the handwritten interface method;
5. convert a successful handwritten response to protobuf; and
6. preserve the current bare-error behavior by returning the generated empty
   response message after a nil error.

The binding contains no `grpc.ServiceRegistrar`, `grpc.ServiceDesc`,
`grpc.ClientConnInterface`, interceptor, or gRPC call option. It is
protobuf-specific executable metadata, not a gRPC registration.

### Immutability

Bindings are generated package data and must be immutable after package
initialization. Accessors must return values or copied slices so callers cannot
change method inventories or replace invokers.

There must be no process-global map from Point IDs to bindings. The binding is
reachable only from the actual Point definition and provider declaration that
uses it.

### One service per Point

The initial implementation must preserve the current rule that an ordinary
publishable Point maps to exactly one complete protobuf service. This keeps
service identity, collision handling, process inventory, and proxy routing
unambiguous.

Supporting several services for one Point is out of scope. If added later, it
must define service identity and partial-registration behavior explicitly.

### Unary methods first

The current generator supports unary Point methods. The first binding API should
model unary requests and responses directly rather than introduce speculative
streaming abstractions.

A later streaming design will need explicit stream direction, element
constructors, conversion, cancellation, and interceptor semantics. It must not
be inferred from the unary binding shape.

## Carrying the binding with a Point

`extensions.Point[T]` currently stores only a `PointID`, and
`Point.Provide(impl)` reduces the Point to `{Point, Impl}`. The new design must
preserve optional Point bindings through provider construction.

Conceptually:

```go
type Point[T any] struct {
	id       PointID
	bindings pointBindings
}

type Provider struct {
	Point PointID
	Impl  any

	// Private immutable reference copied by Point.Provide.
	definition *pointDefinition
}
```

The root `extensions` package should not import gRPC. Prefer a small generic
Point-binding capability or option API so protobuf support remains optional.
The protobuf binding package can identify and validate its own binding type.

Required behavior:

- `Point.Provide` retains the Point definition and its bindings.
- `Point.Dependency` may retain the definition when later client-side automatic
  wiring needs it, but that is not required for the first server-side phase.
- `servicev0.Offer` continues to copy only Point IDs.
- Runtime protocol declarations continue to serialize IDs and service
  inventories, never functions or local binding objects.
- Broker resolution continues to expose only implementations to callers.
- Constructing an internal-only Point without a protobuf binding remains valid.

The API should not expose an untyped mutable `map[string]any`. Use a constrained
binding abstraction with duplicate-kind validation and copied lookup results.

## Defining a protobuf-bound Point

The intended source should still make the ordinary Point declaration obvious:

```go
var Point = extensions.DefinePoint[Greeter](
	"org.mobyproject.extension.example.greeter.v0",
	// Generated protobuf binding attached here or by generated companion code.
)
```

The exact attachment mechanism needs an implementation decision before coding.
It must satisfy all of these requirements:

- no import cycle between `greeterv0` and `greeterv0/protogen`;
- no process-global registry;
- no order-dependent capture of an unbound Point by `Point.Provide`;
- generation can bootstrap a new Point before generated files exist;
- stale generated output fails generation or compilation clearly; and
- the handwritten Point declaration remains the source of the Point ID,
  interface type, and cardinality.

Two viable approaches should be prototyped:

1. **Generated companion attachment.** Make `Point` retain an immutable
   definition reference. Generated code in the same package attaches the
   protobuf binding to that definition during package initialization. Providers
   retain the definition reference rather than an early copy. This is local
   Point construction, not registration in a global catalog, but ordering and
   mutation must be tightly constrained.
2. **Generator-owned bound declaration.** Separate the handwritten Point
   specification from the generated bound `Point` value. The generator emits
   the final Point declaration and binding together. This avoids attachment
   mutation but changes the Point-authoring API and must solve first-generation
   bootstrapping cleanly.

Do not choose an `init()` function that inserts registrations into a package- or
process-global map. Do not make extension declarations call a generated bind or
attach function.

## Generic gRPC server adaptation

`servicegrpc` should become a generic adapter from a protobuf-bound provider to
one gRPC service. Its input is the provider's Point binding and implementation,
not a `serverpoint.Registration`.

Conceptually:

```go
func Adapt(provider extensions.Provider) (Service, error)
```

or, if binding lookup remains outside the package:

```go
func Adapt(binding pointproto.ServiceBinding, impl any) (Service, error)
```

The adapter must:

1. validate that the provider and binding are complete;
2. validate that the protobuf service package equals the Point ID;
3. validate that the full service name is
   `<PointID>.<InterfaceName>`;
4. construct one `grpc.ServiceDesc` from the protobuf descriptor;
5. construct one unary handler per bound method;
6. allocate requests through `NewRequest`;
7. invoke the generated binding function;
8. preserve unary interceptor behavior and canonical full method names;
9. preserve gRPC status errors returned by the implementation;
10. record the resulting fully qualified service name for collision checks; and
11. reject duplicate, missing, or incomplete method bindings before server
    registration.

The generic gRPC wrapper may use a private common handler interface for
`grpc.ServiceDesc.HandlerType`. It must not weaken validation by registering the
raw provider against an empty interface and assuming handlers will never receive
an incompatible value.

The adapter should continue preflighting services before mutating the daemon's
gRPC server. Host startup must detect missing bindings and name collisions before
`RegisterInProcessServices` is called.

## SDK behavior

A process extension already holds local provider declarations with their Point
definitions. The SDK can therefore register ordinary providers without a
`WithServerPoints` option:

```go
sdk.Main(extension)
```

`Server.Register` should:

1. enumerate ordinary providers;
2. ignore `servicev0.Point` as transport-free offer metadata;
3. find each provider's protobuf binding;
4. build and register its gRPC service through the generic adapter;
5. record the service name under the ordinary Point in `ProviderServices`;
6. serialize offered Point IDs separately; and
7. reject an offered Point that has no complete protobuf binding or service
   inventory.

Whether every process provider must have a protobuf binding is a separate policy
choice. The minimal compatible rule is:

- an offered provider requires a binding;
- a provider used across a process boundary requires a binding; and
- a purely local provider may omit one.

If the SDK currently serves every ordinary process provider regardless of use,
retain that behavior initially and require bindings for all process providers.
Narrowing it is a separate behavior change.

Remove `WithServerPoints` and generated server arguments from
`Server.Register` only after equivalent validation and service inventory tests
exist. Keep `sdk.Main(extension, options ...MainOption)` as the extensible SDK
entrypoint.

## Host behavior

### In-process publication

For an allowed in-process offer, the Host already has the provider declaration.
It should obtain the protobuf binding from that provider and use the generic
gRPC adapter.

The effective flow becomes:

```text
implemented provider
    ∩ extension offer
    ∩ Host publication policy
    -> provider protobuf binding
    -> generic gRPC service
    -> collision preflight
    -> public registration
```

`host.Options.PointServers` is removed. Missing binding errors are attributed to
the extension and Point:

```text
extension "..." cannot publish point "...": point has no protobuf binding
```

Policy denial must occur before requiring the binding. An extension may offer an
internal-only or unsupported Point without preventing startup when Host policy
denies that offer.

### Process publication

Process publication remains proxy-based. The Host must not accept executable
bindings from an untrusted process declaration.

The SDK registers the process's local binding, captures the resulting service
name, and reports it in `ProviderServices`. The Host validates offer metadata and
service inventory, applies policy, checks collisions, and proxies approved full
service names exactly as it does now.

No protobuf binding needs to cross the runtime protocol.

### Dependency callback serving

`DependencyProviders` is another server-registration catalog. Once a Host-side
provider retains its protobuf binding, the same generic adapter should be able
to serve it on the private callback boundary.

Removing `DependencyProviders` may be included after public in-process serving
works, but it should be a separate reviewable step. Callback authorization,
provider selection, and lifecycle must remain unchanged.

### Calls to process providers

`ClientPoint` and `ClientProviders` solve the opposite problem: constructing a
concrete handwritten Go interface over a remote connection. Server invocation
metadata alone does not automatically manufacture an arbitrary Go interface at
runtime.

The first split may retain `ClientPoint` and `ClientProviders`. A later extension
of the protobuf binding could include a generated client factory over a
transport-neutral protobuf invoker. A generic gRPC client transport could then
consume that factory when the Host already has the local Point definition.

Do not claim that this server-binding work automatically removes all client
catalogs.

## `grpcgen` responsibilities

After the split, generated `grpcgen` code should contain APIs that genuinely need
gRPC:

- raw protobuf gRPC client interfaces;
- typed client constructors over `grpc.ClientConnInterface`;
- method path constants if they are part of the supported generated API;
- optional direct server registration for callers outside the framework; and
- initially, `ClientPoint` and `NewClient` for internal process calls.

It should not be required for framework server publication. In particular, the
Host and SDK should not import each Point's `grpcgen` package merely to register
a server.

Conversions used by both the protobuf binding and typed gRPC clients should have
one generated implementation. Do not duplicate conversion algorithms between
the Point package and `grpcgen`, because the copies can drift.

Possible arrangements include exported generated codec functions in the Point
package or a generated immutable codec object consumed by `grpcgen`. Choose the
smallest API that avoids exposing unnecessary conversion internals.

## Protocol and service identity

No new runtime protocol field is required.

The existing process declaration continues to carry:

```go
type Declaration struct {
	ID               string
	Providers        []PointDeclaration
	Dependencies     []Dependency
	Conflicts        []string
	OfferedPoints    []string
	ProviderServices []ProviderServices
}
```

The SDK derives `ProviderServices` from the generic gRPC registration rather
than from supplied `ServerPoint` values.

The generator and adapter must validate all three representations of identity:

```text
Point ID                         org.example.greeter.v1
protobuf package                 org.example.greeter.v1
protobuf/gRPC service            org.example.greeter.v1.Greeter
```

A mismatch is a generation or startup error. There is no service-name override.

## Validation and security

Preserve or add these checks:

- invalid Point IDs fail at Point definition;
- duplicate binding kinds on one Point fail deterministically;
- a protobuf binding has exactly one service;
- every protobuf service method has exactly one generated method binding;
- no generated method binding names an unknown descriptor method;
- request constructors return the expected concrete protobuf type;
- method invocation rejects an incompatible provider implementation;
- an offered Point is implemented by the same extension;
- a policy-approved in-process offer has a complete protobuf binding;
- policy-denied offers do not require a binding;
- process offers have non-empty generated service inventory;
- service names cannot collide across process and in-process extensions;
- extension services cannot shadow `ReservedServices`; and
- public inventory contains only offer-and-policy-approved services.

Bindings are trusted executable code linked into the daemon or extension binary.
Runtime declarations from launched processes remain untrusted data and must not
select arbitrary binding implementations, Go symbols, or package paths.

The split does not add authentication or authorization to a published service.
A service exposed on the raw daemon gRPC endpoint must still enforce any access
control required by its contract.

## Generator changes

`mobyextgen` currently emits protobuf messages, gRPC handlers, adapters, and
conversions into `protogen`. Split generation in stages.

### Protobuf output

Generate only protobuf data in `protogen`:

```text
protogen/<service>.pb.go
```

It should contain generated message types, file descriptors, service
descriptors, and protobuf reflection data. It must not import the handwritten
Point package, gRPC, `extensions`, `serverpoint`, or `clientpoint`.

### Point-package binding output

Generate a file in the handwritten Point package, for example:

```text
proto.bind.gen.go
```

It should contain:

- domain/protobuf conversion functions;
- immutable service and method bindings;
- generated invokers against the handwritten interface; and
- the selected Point attachment mechanism.

This file imports `protogen` and protobuf binding support, but not gRPC.

Non-Point runtime services generated with `-service` have no ordinary Point to
carry a binding. Keep their explicit generated gRPC registration path unless a
separate service-contract abstraction is designed. Do not force the SDK runtime
protocol into the Point model.

### gRPC output

Generate gRPC-specific code in:

```text
grpcgen/wire.gen.go
```

Initially move raw clients, `ClientPoint`, and `NewClient` there. Remove
framework `ServerPoint` generation after the generic server adapter provides the
same behavior.

### Stale files

The generator must remove or reject stale output from the previous combined
layout. A successful generation must not leave an old
`protogen/wire.gen.go` that still registers a second service.

Deletion targets must be limited to files carrying the generator's exact
header. Never remove an unknown user-owned file only because its name matches an
old generated path.

## Migration sequence

Keep package movement, binding behavior, and API removal reviewable.

### Phase 1: Protobuf-only `protogen`

1. Split protobuf message/descriptor emission from current wire emission.
2. Add `grpcgen` output with behavior equivalent to the current generated wire
   package.
3. Migrate checked-in generated imports and fixtures.
4. Keep `ServerPoint`, `ClientPoint`, and current composition APIs working from
   `grpcgen`.
5. Verify generated output reproducibility.

This phase is intended to be behavior-neutral.

### Phase 2: Point protobuf binding

1. Add the optional immutable Point-binding abstraction.
2. Preserve binding references through `Point.Provide`.
3. Generate protobuf conversions and server invokers in the Point package.
4. Validate service identity and method completeness.
5. Keep old `grpcgen.ServerPoint` temporarily as a reference implementation.
6. Add equivalence tests between old generated registration and the new binding.

### Phase 3: Generic SDK server

1. Teach the SDK to adapt ordinary providers from protobuf bindings.
2. Compare generated service names and observable RPC behavior with
   `grpcgen.ServerPoint`.
3. Remove `WithServerPoints` and server registration arguments from
   `Server.Register` while retaining functional options on `sdk.Main`.
4. Remove SDK dependencies on `serverpoint.Registration`.
5. Retain current declaration and service inventory wire shapes.

### Phase 4: Generic Host server

1. Adapt policy-approved in-process providers from their protobuf bindings.
2. Preserve startup preflight and collision behavior.
3. Remove `host.Options.PointServers`.
4. Adapt private dependency callback serving from provider bindings.
5. Remove `host.Options.DependencyProviders` only if callback tests prove
   equivalent behavior.

### Phase 5: Remove generated server registrations

1. Remove `ServerPoint` from Point `grpcgen` output.
2. Remove the root `serverpoint` package if no non-Point or compatibility use
   remains.
3. Remove obsolete generated imports and documentation.
4. Regenerate every checked-in Point fixture.
5. Search for stale `ServerPoint`, `PointServers`, and SDK server-registration
   arguments.

### Phase 6: Optional generic client binding

Treat client-side discovery separately.

1. Define a transport-neutral protobuf unary invoker.
2. Generate a concrete handwritten-interface client factory in the Point
   binding.
3. Adapt it to `grpc.ClientConnInterface` in generic gRPC client code.
4. Remove `ClientPoint` or `ClientProviders` only where a local Point definition
   supplies equivalent typed construction.
5. Preserve explicit failure for unknown process Points the Host cannot call.

## Verification

Each phase needs an independent observable oracle.

### Generator

- Generate from a clean fixture with no prior output.
- Regenerate existing output and compare hashes or `git diff`.
- Confirm `protogen` does not import the Point package or gRPC.
- Confirm the Point package's generated binding does not import gRPC.
- Confirm `grpcgen` imports flow in the allowed direction.
- Confirm stale combined output is removed safely.
- Confirm non-Point `-service` generation remains functional.

### Binding

- Call every supported method shape through the binding.
- Cover bare-error and request/response methods.
- Cover every supported scalar, nested, repeated, map, and byte field shape.
- Compare results with a direct handwritten implementation call.
- Reject wrong implementation and protobuf request types.
- Reject missing, duplicate, and mismatched descriptors or methods.

Tests should assert RPC behavior and conversion results, not private binding
field layout or generated helper names.

### Generic gRPC adapter

- Serve a protobuf-bound in-process provider and call it with a generated raw
  gRPC client.
- Verify interceptor invocation and full method names.
- Verify metadata, status errors, cancellation, and malformed request behavior.
- Compare service inventory with the current generated `ServerPoint` path.
- Reject a Point with no protobuf binding before registering any service.
- Reject reserved-name and duplicate-service collisions before registration.

### SDK and process path

- Start a process extension with `sdk.Main(extension)` and no registrations.
- Verify `Describe` reports the same provider service inventory.
- Proxy an allowed offered Point through the Host socket.
- Verify denied offers remain unreachable.
- Verify an offered provider without a binding fails process startup.
- Verify malformed process declarations remain rejected by the Host.

### Repository checks

Run generation before tests so checked-in output is the tested output:

```fish
go generate ./example/greeter/v0 ./internal/launcher/echo/v1 ./sdk/sdkapi
go test ./...
go vet ./...
```

Also run the repository verification tool on each changed package after every
meaningful implementation phase.

## Moby integration impact

The target Moby Host composition no longer imports generated server adapters for
in-process publication:

```go
host.Options{
	Extensions:       installedExtensions,
	AllowPublication: publicationPolicy,
	ReservedServices: daemonServices,
}
```

Moby still needs to:

- define publication policy;
- reserve daemon-owned gRPC service names;
- register preflighted in-process extension services on the daemon gRPC server;
- install proxy routes for approved process service inventories; and
- use generated `grpcgen` clients where daemon or external client code makes
  typed gRPC calls.

Adding a new protobuf-bound Point no longer requires adding its server adapter
to a Host catalog. Adding a new public API still requires an explicit policy
decision.

## Rejected alternatives

### Extension-owned `ServerPoint`

This makes the extension declaration or implementation transport-aware and
conflates implementation with serving mechanics.

### `servicev0.Offer` carrying a registration

An offer is metadata and not authorization. Carrying executable registration
would restore the publication binding that Host-owned publication deliberately
removed.

### Process-global adapter registry

Generated `init()` registration hides linked capabilities, complicates tests,
and makes imports mutate global framework behavior.

### Naming-convention import of `grpcgen`

Go cannot import a package from a runtime string. Package paths and symbols must
be linked at compile time.

### Protobuf descriptors without invokers

Descriptors can allocate and inspect wire messages, but they cannot call an
arbitrary handwritten Go interface or infer semantic domain/protobuf
conversions.

### Reflection over Point implementations

Method names and Go parameter types are insufficient to reconstruct protobuf
field mapping, empty-response behavior, future compatibility rules, or generated
message constructors.

### Protobuf types in the handwritten interface

Making every Point method accept generated protobuf messages would simplify
server dispatch but would make the Point implementation serialization-specific.
The proposed binding preserves handwritten domain types and keeps protobuf at
the contract boundary.

### Dynamic Go plugins

Plugins add platform, toolchain, dependency-identity, deployment, and unloading
constraints and do not fit static daemon builds.

## Open decisions

Resolve these before implementation:

1. How generated binding data attaches to the handwritten `Point` without a
   global registry or fragile initialization order.
2. Whether the root `extensions` package exposes a generic binding capability or
   a narrower internal mechanism consumed through a protobuf support package.
3. Whether every out-of-process provider requires a protobuf binding, or only
   providers that cross a process/publication boundary.
4. Whether conversion functions are exposed for `grpcgen` reuse or hidden behind
   a generated codec object.
5. Whether direct raw server registration remains in `grpcgen` after framework
   `ServerPoint` removal.
6. Whether dependency callback serving is migrated with public Host serving or
   in a separate phase.
7. Whether client factories join the Point protobuf binding later, allowing some
   `ClientPoint` catalogs to be removed.
8. How the generator bootstraps a brand-new Point before its generated binding
   file exists.

These decisions must not weaken the primary ownership rule: extension code owns
the implementation and publication offer, the Point contract owns protobuf wire
mapping, and the Host owns transport selection and publication authority.

## Acceptance criteria

The work is complete when:

- `protogen` contains protobuf messages and descriptors only;
- the Point package carries generated protobuf invocation metadata without
  importing gRPC;
- extension implementations and declarations name no protobuf or gRPC APIs;
- `servicev0.Offer` remains metadata-only;
- the SDK serves process providers without supplied `ServerPoint` values;
- the Host serves approved in-process offers without `PointServers`;
- no process-global adapter registry or generated blank-import catalog exists;
- denied offers do not require a server binding;
- approved offers without a complete binding fail before registration;
- process service inventory and proxy routing remain unchanged on the wire;
- collision and reserved-service checks remain fail-closed;
- internal in-process Point calls remain direct Go calls;
- generated external gRPC clients remain available from `grpcgen`;
- generated output is reproducible; and
- repository generation, tests, vet, and scoped verification pass.
