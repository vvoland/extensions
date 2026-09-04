# Host-owned publication of extension Points

## Goal

Allow an extension to offer selected ordinary Points as external APIs while the
extension Host retains the final publication decision.

The model has three distinct responsibilities:

1. An ordinary Point defines the typed API and its provider implementation.
2. The extension declares which of its implemented Points are eligible for
   external publication.
3. The Host applies policy and chooses which offered Points are reachable on its
   public API connection.

The extension declaration must not import protobuf or gRPC packages merely to
offer a Point. Generated transport adapters belong at SDK and Host composition
boundaries.

This repository and protocol are experimental. No backward-compatibility
promise applies yet, so obsolete fields and APIs may be removed or renumbered
without migration shims.

## Do we split `protogen` first?

No. Splitting generated protobuf messages from generated gRPC wiring is not a
prerequisite for the publication model.

The current generated package already exposes the required composition APIs:

- `ServerPoint` to serve an ordinary Point;
- `ClientPoint` to construct an internal provider over a connection; and
- `NewClient` to construct the handwritten interface over a connection.

The extension implementation and declaration can avoid importing that package.
Only the process entrypoint or Host composition code needs it:

```go
sdk.Main(compose.Extension, sdk.WithServerPoints(composewire.ServerPoint))
```

Use a `composewire` import alias in composition code to make the transport
boundary explicit even while the package path remains `protogen`.

Splitting generated output first would create a large package and API migration
without resolving the Host-policy problem. Defer it until the publication model
is complete and stable.

A later optional split is described in [Generated package split](#generated-package-split).

## Final source API

### Ordinary API Point

Compose API remains an ordinary Point:

```go
type API interface {
	Up(context.Context, *UpRequest) (*UpResponse, error)
}

var Point = extensions.DefineSinglePoint[API](
	"com.docker.compose.api.v1",
)
```

`mobyextgen` infers the gRPC service name as
`com.docker.compose.api.v1.API`. The interface name is part of the wire identity
and remains stable within that Point version.

### Extension implementation and offer

The extension provides the ordinary Point and separately offers that Point for
Host-controlled publication:

```go
var Extension = extensions.New(extensions.Declaration{
	ID: "com.docker.compose.v1",
	Providers: []extensions.Provider{
		composev1.Point.Provide(composeAPI{}),
		servicev0.Offer(composev1.Point),
	},
})
```

`servicev0.Offer` means “this implemented Point may be published.” It does not
select gRPC, register a server, or force the Host to expose anything.

### Process composition

The process entrypoint supplies the generated transport adapter for every
ordinary Point it serves:

```go
func main() {
	sdk.Main(
		compose.Extension,
		sdk.WithServerPoints(composewire.ServerPoint),
	)
}
```

This is the only extension-side layer that imports generated transport wiring.

### External client

An external client uses the generated handwritten client against the Host
connection:

```go
client := composewire.NewClient(hostConn)
reply, err := client.Up(ctx, request)
```

## `servicev0.Point` becomes offer metadata

Replace the current binding and registration API with a transport-neutral
metadata Point.

Proposed API:

```go
package servicev0

type Provider interface {
	OfferedPoints() []extensions.PointID
}

var Point = extensions.DefinePoint[Provider](
	"org.mobyproject.extension.service.v0",
)

type offeredPoint interface {
	ID() extensions.PointID
}

func Offer(points ...offeredPoint) extensions.Provider
```

`Offer` must:

- require at least one value exposing a Point ID;
- reject duplicate Points and `servicev0.Point` itself;
- copy the resulting Point IDs;
- return one provider of `servicev0.Point`; and
- expose no transport-specific types.

Remove the current publication machinery:

- `servicev0.Binding`;
- `servicev0.Bind`;
- transport registrations stored in `servicev0`;
- `servicev0.Expose`;
- generated `Bind` helpers; and
- automatic registration through `servicegrpc.ServerPoint`.

The Host and SDK must validate that every offered Point is also implemented by
the same extension.

## Separate-process protocol

The process handshake needs to carry offered Point IDs explicitly. Point IDs are
already stable identifiers, so `containerd/typeurl` is unnecessary.

Add `OfferedPoints` to `sdkapi.Declaration`:

```go
type Declaration struct {
	ID               string             `pb:"1"`
	Providers        []PointDeclaration `pb:"2"`
	Dependencies     []Dependency       `pb:"3"`
	Conflicts        []string           `pb:"4"`
	OfferedPoints    []string           `pb:"5"`
	ProviderServices []ProviderServices `pb:"6"`
}
```

Replace the obsolete `ExposedServices` field with `OfferedPoints`. No legacy
field reservation or conversion is required while the protocol is experimental.

Update:

- `sdk/sdkapi/runtime.go` and generated protocol files;
- `internal/extensiondecl.Declaration` and `Parse`;
- `internal/launcher.Launched`;
- launcher declaration conversion and validation; and
- hosted runtime adapters.

Validation rules:

- every offered Point ID is valid and unique;
- every offered Point appears in the same extension's provider declaration;
- `servicev0.Point` cannot offer itself;
- process service inventory for an offered Point is grouped under that ordinary
  Point, not under `servicev0.Point`; and
- malformed offers fail extension startup.

## SDK behavior

`servicev0.Point` is declaration metadata and has no gRPC service of its own.

Change `sdk.Server.Register` as follows:

1. Detect the extension's `servicev0.Point` provider.
2. Read and validate its offered Point IDs.
3. Store those IDs in `sdkapi.Declaration.OfferedPoints`.
4. Do not require or run a `ServerPoint` for `servicev0.Point`.
5. Register each ordinary provider through the generated `ServerPoint` supplied
   to `sdk.Main` or `Server.Register`.
6. Record generated service names under the ordinary Point in
   `ProviderServices`.
7. Require every offered process Point to have a generated server registration
   and non-empty service inventory.

The current physical-registration deduplication for a Point that is both
provided and published becomes unnecessary. Each ordinary Point is registered
once; publication is a later Host decision.

Expected process declaration:

```text
providers:
  - com.docker.compose.api.v1
  - org.mobyproject.extension.service.v0

offered points:
  - com.docker.compose.api.v1

provider services:
  com.docker.compose.api.v1:
    - com.docker.compose.api.v1.API
```

## Host policy

An offer is not authorization. The Host must apply an explicit policy, with deny
as the default.

Proposed option:

```go
type PublicationPolicy interface {
	Allow(extension extensions.ExtensionID, point extensions.PointID) bool
}

type PublicationPolicyFunc func(
	extension extensions.ExtensionID,
	point extensions.PointID,
) bool

func (f PublicationPolicyFunc) Allow(
	extension extensions.ExtensionID,
	point extensions.PointID,
) bool

type host.Options struct {
	// Existing fields...
	AllowPublication PublicationPolicy
}
```

A nil policy denies publication.

The policy may consider daemon configuration, extension identity, Point ID,
licensing, authentication mode, or product-specific rules. The framework must
not hard-code Compose or other service identities.

The effective public set is:

```text
extension-offered Points ∩ Host-allowed Points
```

## Host loading and validation

### Process extensions

A process extension may provide a Point that has no `ClientPoint` registered in
the Host when that Point is offered for publication. This is valid because the
Host proxies the service and does not construct an internal caller.

Change hosted-extension adaptation:

- always recognize `servicev0.Point` as metadata; it does not need
  `ClientProviders` or `ExposeOnlyPoints` wiring;
- accept an ordinary provider without `ClientPoint` only when it is listed in
  `OfferedPoints`;
- continue rejecting unknown, non-offered Points;
- build an internal provider normally when a `ClientPoint` is available, even if
  the Point is also offered;
- retain private connection and service inventory for offered Points; and
- apply `AllowPublication` before returning public routes.

The existing `ExposeOnlyPoints` option was introduced for publication carrier
Points. Remove it if no other non-public caller uses remain after migration.

### Public inventory API

Do not expose every private service inventory entry as public.

Replace or narrow `Host.ServicesForPoint` so callers can obtain only effective
published services. A clear API is preferable:

```go
func (h *Host) PublishedServicesForPoint(
	point extensions.PointID,
) map[extensions.ExtensionID][]string
```

The method returns services only when:

- the extension offered the Point;
- Host policy allowed that extension and Point; and
- the SDK reported generated services for that Point.

Keep raw private inventory internal to the Host.

## In-process extensions

A process extension owns a private gRPC server, so its SDK-side generated
`ServerPoint` is sufficient. An in-process provider has no private server to
proxy, so the Host needs the generated server adapter locally.

Transport adapters are Host capabilities, not extension publication decisions.
Add explicit Host composition wiring, for example:

```go
type host.Options struct {
	// Existing fields...
	PointServers []serverpoint.Registration
	ReservedServices []string
}
```

For a built-in Compose extension:

```go
host.New(ctx, host.Options{
	Extensions: []extensions.Extension{
		compose.Extension,
	},
	PointServers: []serverpoint.Registration{
		composewire.ServerPoint,
	},
	AllowPublication: policy,
})
```

For each allowed in-process offer, the Host must:

1. resolve the offered Point implementation from the same extension;
2. find the generated `ServerPoint` adapter by Point ID;
3. fail startup if the adapter is missing;
4. collect the generated gRPC service descriptor before registration;
5. apply reserved-name and duplicate-service checks; and
6. retain the prevalidated service for daemon registration.

After `Host.New` succeeds, daemon composition registers those services:

```go
h.RegisterInProcessServices(grpcServer)
```

`ReservedServices` supplies daemon-owned names to the same collision preflight
used for process and in-process extension services.

The root `servicegrpc` package may remain as a Host-side helper, but its API
should accept an ordinary `serverpoint.Registration` and implementation. It
must no longer depend on `servicev0.Binding` or treat the metadata Point as a
transport registration.

## Route selection and cardinality

A public gRPC service name can route to only one backend.

Rules:

- Prefer `DefineSinglePoint` for externally callable service Points.
- If several extensions offer the same Point, Host policy must allow exactly one
  provider or startup fails with a collision.
- A published service name cannot shadow a daemon-owned or in-process service.
- Process/process, process/in-process, and extension/daemon collisions all fail
  startup rather than selecting by load order.
- Built-in provider precedence used for internal Point resolution does not imply
  publication precedence.

## Security

Publication bypasses REST authorization-plugin coverage and reaches the gRPC
endpoint directly.

Required controls:

- Host policy defaults to deny.
- The Host validates that offered Points are implemented by the offering
  extension.
- Reserved daemon service names cannot be published.
- Service implementations enforce their own authentication and authorization.
- Extension identity and Point identity are available to policy decisions.
- Denied offers do not prevent the extension from loading unless product policy
  explicitly requires that behavior.

## Generator changes

After Host-owned offers are implemented:

- stop importing `servicev0` in generated ordinary Point packages;
- remove generated `Bind`;
- keep `ServerPoint`, `ClientPoint`, and handwritten `NewClient`;
- always infer a Point's service name from its interface, with no override; and
- retain explicit fully qualified service generation only for existing internal
  runtime contracts that are not Points.

Regenerate Greeter, Echo, and all checked-in Point fixtures.

## Generated package split

This is optional follow-up work, not phase one.

If package clarity is still a problem after composition imports are isolated,
split generated output into:

```text
protogen/
  <service>.pb.go     # protobuf messages and descriptors only

grpcgen/
  wire.gen.go         # gRPC handlers, clients, conversions, ServerPoint,
                      # ClientPoint, and NewClient
```

The `grpcgen` package would import both the handwritten contract package and
`protogen`. The protobuf package would not import the handwritten contract,
avoiding an import cycle.

This split is a public generated-API move. Handle it in its own commit with:

- regenerated fixtures;
- import migration from `.../protogen` to `.../grpcgen` for transport APIs;
- protobuf-only users remaining on `protogen`;
- generated-source reproducibility checks; and
- explicit release notes if downstream consumers already use generated paths.

Do not use `typeurl` to hide this dependency. A type URL can identify serialized
data, but it cannot transfer or reconstruct executable `ServerPoint.Register`
behavior. The receiving process must still link the generated adapter.

## Implementation sequence

### Phase 1: Offer metadata and protocol

1. Replace `servicev0.Bind`/`Expose` internals with `servicev0.Offer` metadata.
2. Replace `ExposedServices` with `OfferedPoints` in the SDK protocol.
3. Regenerate SDK protocol files.
4. Parse and validate offered Points in `internal/extensiondecl` and launcher.
5. Add focused protocol and validation tests.

Verification:

```fish
go test ./extpoints/service/v0 ./sdk/sdkapi ./internal/extensiondecl ./internal/launcher
```

### Phase 2: SDK registration

1. Special-case the metadata-only `servicev0.Point` provider.
2. Register ordinary providers exactly once using supplied `ServerPoint` values.
3. Record service inventory under actual Point IDs.
4. Serialize offered Point IDs.
5. Remove publication-specific physical deduplication.
6. Add publication-only, internal-only, and combined-provider tests.

Verification:

```fish
go test ./sdk
```

### Phase 3: Process Host policy

1. Carry offered Points into `launcher.Launched` and hosted runtime state.
2. Add default-deny `AllowPublication` policy.
3. Accept offered Points without `ClientPoint` while rejecting unsupported,
   non-offered Points.
4. Filter public service inventory by offer and policy.
5. Rename or replace `ServicesForPoint` with a publication-safe API.
6. Add allow, deny, unknown Point, missing inventory, and collision tests.

Verification:

```fish
go test ./host ./grpcproxy ./internal/launcher
```

### Phase 4: In-process publication

1. Add Host-side generated `PointServers` registration.
2. Resolve offered implementations by extension and Point.
3. Apply Host policy before registration.
4. Fail startup for allowed offers without a local server adapter.
5. Apply one collision path across daemon, in-process, and process services.
6. Add in-process and mixed-backend tests.

Verification:

```fish
go test ./host ./servicegrpc ./grpcproxy
```

### Phase 5: Remove extension-side transport binding

1. Remove generated `Bind` and generated `servicev0` imports.
2. Remove obsolete `servicev0.Binding`, transport registrations, and
   `servicev0.Expose`.
3. Refactor root `servicegrpc` into a Host-only ordinary Point adapter.
4. Migrate Greeter and Compose-style fixtures to `Point.Provide` plus
   `servicev0.Offer`.
5. Update authoring and design documentation.
6. Search for removed APIs and stale publication examples.

Verification:

```fish
go generate ./example/greeter/v0 ./internal/launcher/echo/v1
go test ./...
go vet ./...
```

### Phase 6: Moby integration

In the Moby repository:

1. Supply the Host publication policy.
2. Allow the intended Compose API Point and deny unapproved offers.
3. Supply generated `ServerPoint` adapters for allowed in-process services.
4. Build proxy routes from effective published process inventories.
5. Register allowed in-process services after collision preflight.
6. Construct external generated clients over the Engine connection.
7. Add daemon-socket integration tests for allowed, denied, and colliding
   services.

### Phase 7: Optional generated-package split

Only after phases 1–6 are stable, decide whether the dependency clarity is worth
the generated import migration. If yes, split `protogen` and `grpcgen` in a
separate behavior-neutral change.

## Commit structure

Keep independently reviewable boundaries:

1. `sdkapi: Carry offered extension Points`
2. `sdk: Serve offered ordinary Points`
3. `host: Apply policy to offered Point services`
4. `host: Register allowed in-process Point services`
5. `mobyextgen: Remove extension-side publication bindings`
6. `docs: Describe Host-owned Point publication`
7. Optional: `mobyextgen: Split protobuf and gRPC generated packages`

Do not combine the optional generated-package move with protocol or publication
behavior changes.

## Final acceptance criteria

- Extension implementation and declaration import no generated protobuf or gRPC
  package merely to offer a Point.
- The process entrypoint or Host composition explicitly supplies generated
  transport adapters.
- Every published API is an ordinary Point implemented by the same extension.
- The extension can offer a subset of its Points.
- The Host can deny any offer and defaults to deny.
- Only Host-approved offers appear on the public API socket.
- Internal Point resolution is unchanged.
- Process and in-process publication follow the same collision and authorization
  policy.
- No `typeurl` or global adapter registry is required.
- Generated output is reproducible.
- `go test ./...` and `go vet ./...` pass in this repository and in the Moby
  integration change.
