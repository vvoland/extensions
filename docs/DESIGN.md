# Extensions — Design

This document is the authoritative description of current behavior and constraints.
See the [overview](../README.md#how-it-works) and procedural [authoring guide](./AUTHORING.md).
Runtime reload, out-of-process health handling, and scoped dependency resolvers are not current behavior.
The Go API and runtime protocol are experimental and currently make no backward-compatibility promise.
The framework is a standalone Go module embedded through `host.New`.
References to a daemon below mean the embedding application, which supplies configuration, discovery directories, and any public transport endpoint.
Docker configuration files, CLI flags, and engine integration are outside this module.

## Contract and startup

An extension declaration contains its logical id, points, dependencies,
conflicts, and optional `Init` and `Shutdown` functions.
The host checks it before calling a provider or wiring an engine flow.
The declared id is not the complete runtime identity: the host validates it
against host authority and combines it with a host-attested origin.

- An in-process declaration is a value read by the host.
- For a separate binary, the daemon must launch the process before obtaining its declaration through the startup `Describe` handshake.
  A rejected declaration cannot provide a point or participate in an engine flow.
- The host assigns origin `builtin` to configured in-process extensions and
  `executable` to binaries it launches.
  An external process cannot declare or otherwise supply its origin; origin is
  absent from the process protocol.
- Configuration is keyed by extension id.
  In-process extensions receive it during `Init`; separate binaries receive it in the startup handshake.
  The id is also the binary name, so the daemon can select configuration before launch.

Registration is explicit: importing a package does nothing, and there is no package-level `func init()` registry.
The host chooses the active set, which is fixed for that host and daemon
lifetime.

## Identifiers

These names are distinct:

- **Extension id** is the deployed unit's versioned reverse-DNS name, for example `com.docker.compose.v1`.
  It has at least two lowercase, dot-separated name segments and ends in a required `v0` or `v12`-style version.
  Segments are alphanumeric with optional internal hyphens; they cannot start or end with a hyphen or contain path separators, `..`, uppercase, or other path- or shell-hostile characters.
  The id is the binary name, configuration key, dependency name, and named-lookup selector, not an interface name.
  Registration rejects invalid ids.
  Its version is a namespace element, not a semantic version, and is independent of point versions.
  Thus `com.foo.v1` and `com.foo.v2` are different extensions, binaries, and configurations that can coexist.
- **Extension identity** is the logical extension id plus the host-attested
  origin `builtin` or `executable`.
  Registration rejects an empty or unknown origin, an invalid identity id, or
  an identity id that differs from the declaration id.
  Providers and Host policy receive this validated identity.
- **Point id** identifies a versioned interface contract, for example `com.docker.compose.api.v1`.
  It is a lowercase, dot-separated, reverse-DNS-style name ending in `vN`; segments may contain digits, hyphens, and underscores.
  Providers implement points, and point dependencies name their ids.
- **Proto package / gRPC service** is generated from an ordinary Point.
  The point id becomes the `.proto` package, and the service is named `<PointID>.<InterfaceName>`.
  For example, point `org.mobyproject.extension.container.create_hook.v0` generates service `org.mobyproject.extension.container.create_hook.v0.ContainerCreateHook` and wire methods of the form `/<PointID>.<InterfaceName>/Method`.
  Both the point id and interface name are wire identifiers and remain stable within that Point version.
  Socket routing uses the full service name, never the bare point id.
- **CLI / API route** is a separate client-facing name, such as `docker compose up` or a REST path; the framework does not generate it.

Publishing a point id means publishing the generated gRPC service in its proto
package. Socket routing takes the full service name. `servicev0.Point` carries
the extension's offers, while policy on `servicev0.Point.ID()` makes the final
publication decision.

## Resolution, ordering, and lifecycle

Providers are selected when a point is used:

- Named lookup fails if that extension does not provide the point.
- Single-provider lookup fails unless exactly one effective provider exists.
- All-provider lookup returns every provider.
  An extension implements a point at most once, but many extensions may implement the same point.
- Fan-out order is undefined, even if an implementation appears to preserve registration order.
  A point that needs order must define it, preferably as order-independent phases such as `Update` then `Validate`.
- Blocking, fire-and-forget, and veto semantics belong to the point contract.

`DefineSinglePoint` makes cardinality part of the point contract.
Its generated `ClientPoint` carries that fact, so the host rejects two executable providers for the point at startup.
For a single-provider point, a built-in is used only when no executable extension provides the point; an executable provider replaces it without a disable declaration.
Runtime fallback and failure behavior belongs to each point's call helper and contract.
Fan-out accessors include built-in and executable providers and expose each
provider's identity; by-id lookup always honors the named extension id.

Sole ownership is not a general provider setting.
A fan-out point accepts any number of providers, and a consumer may request one at use time.
Socket-exposed services have a different rule: each service name is one public address, so the host rejects a second provider for that name.

Conflicts are between extensions, not points.
Either extension listing the other is sufficient; the broker rejects the whole incompatible set and reports the reason instead of choosing a winner or substituting another extension.

Dependencies are resolved topologically before the dependent initializes.
Missing required dependencies and cycles fail fast; optional dependencies may be absent.

- A **point dependency** requires at least one provider.
  In-process callers may later use ordinary point lookup for one, all, or a provider by id.
- An **extension dependency** names one extension, which initializes first.
  The dependent may rely only on its presence or call it through its points.

An extension dependency adds naming and ordering, not another callable surface: all calls still use points.
During in-process `Init`, the provider is called directly.
During out-of-process `Init`, the extension calls a daemon callback channel that routes to the provider.
The callback currently serves one effective provider per dependency point;
cross-process `All` and by-id selection are deferred.

The lifecycle is register, resolve, initialize, run, and shut down.
Normal shutdown runs semantic extension shutdown in reverse dependency order,
then closes launched extension processes and callback resources.
It tears down only initialized extensions semantically, while still releasing
all acquired process resources.
A launch, policy, registration, or initialization failure unwinds started
processes and initialized extensions; loading is all-or-nothing, not degraded.
Errors are attributed to the extension that produced them.

There is no watchdog, process restart, reconnect, or adoption of extension
processes from an earlier host.
A daemon restart constructs a new Host and a new fixed extension set.
The framework provides no integration with Moby live restore to retain extension processes, callback sockets, or in-memory extension state.
Until a separate extension lifecycle exists, an extension must not own runtime
state required by containers that are expected to survive their host daemon.
The generic Host makes no universal guarantee that an abrupt host crash kills
every child process; OS-specific parent-lifetime mechanisms may provide
additional cleanup.

## Socket publication

An extension is reachable through daemon-internal point calls, or through the
generated gRPC service of an ordinary Point that it explicitly publishes.
Publication does not require the daemon to import the extension's proto.

### Generated Point publication

The source is an ordinary `extensions.DefinePoint` or
`extensions.DefineSinglePoint` with the provider interface and message
structs. `mobyextgen` always infers `<PointID>.<InterfaceName>`.

The generated Point package exports `ServerPoint`, `ClientPoint`, and
handwritten-interface `NewClient(conn)`:

- `ServerPoint` serves an ordinary point to an SDK server or a dependency callback.
- `ClientPoint` builds the host-side provider adapter for a launched ordinary point.
- `NewClient(conn)` returns the handwritten Go interface over a gRPC connection.

The extension implements ordinary Points and separately offers a selected
subset as metadata:

```go
Providers: []extensions.Provider{
	greeterv0.Point.Provide(greeter{}),
	servicev0.Offer(greeterv0.Point),
}
```

`servicev0.Offer` carries no transport registration and grants no publication
authority. The Host applies `WithProviderPolicy` to `servicev0.Point.ID()` and
drops all offers when the policy is nil.
The same policy admits providers wired for internal Host use by ordinary Point
ID; a nil policy preserves their registration.
An `Allow` decision keeps the provider use, `Drop` omits it, and `Reject` fails
Host construction while preserving its error cause.
The zero result rejects as invalid or unspecified, and a typed nil
`PointPolicyFunc` rejects with a generic cause.
An offered-only process Point without Host client wiring is governed only by
the servicev0 policy decision.
The servicev0 marker is exempt from provider admission and controls publication
of the extension's offered Points.

An out-of-process extension passes every ordinary provider's generated
`ServerPoint` to `sdk.Main` or `Server.Register`. The SDK records service names
under the ordinary Point and serializes offered Point IDs separately. The Host
may omit `ClientPoint` wiring for an offered-only Point; add it when the daemon
also calls that Point internally.

For in-process offers, Host composition supplies generated adapters through
`WithPointServers`. The Host resolves the implementation from the same extension,
applies policy, and preflights daemon-reserved, process, and in-process service
name collisions before registration.

External callers use the generated client directly over an existing host or
proxy connection:

```go
client := greeterpb.NewClient(hostConn)
reply, err := client.Greet(ctx, &greeterv0.HelloRequest{Name: "world"})
```

For an out-of-process extension, the SDK registers generated services on the
extension server and reports their fully qualified names. The daemon proxies
matching calls by name and forwards the raw gRPC stream, including metadata and
status.

Generated service names are captured from generated registration, not listed manually.
The extension must provide the shared publication point before publishing its
service inventory. A name cannot collide with another extension or a service
already served by the daemon; collision fails startup rather than shadowing the
existing service.

The Host does not require a published service to be named `<PointID>.<Service>`.
Which services a Point carries is part of its contract, not something the framework enforces.
It is up to Host policy to reject Points that publish services outside their proto package.


The embedding application chooses the gRPC endpoint used for publication; the framework does not create a Docker API endpoint.
An exposed service must enforce any access control it needs.
In a Moby integration, REST authorization plugins must not be assumed to protect a separate gRPC endpoint.

## Separate-process protocol

The binary name is its extension id (`<id>.exe` on Windows), and the daemon launches it from the configured extension directory.
Startup proceeds as follows:

1. The daemon writes JSON to stdin with `endpoint`, `protocolVersion`, `config`, and, when dependencies are offered, `callbackEndpoint`.
   The current protocol version is `1`; `config` is the entry supplied through `host.WithExtensionConfig` for the id.
2. The SDK listens on the supplied Unix socket, then writes exactly `ready\n` to stdout.
   Stdout is reserved for this line: any earlier output corrupts the handshake and fails launch.
   The daemon also drains later stdout so it cannot block the process.
3. The daemon captures stderr in its logs, dials the Unix socket, and calls the runtime `Describe` RPC.
   The returned id must match the binary name.
   Before registration, the daemon validates the declaration, including provider points, dependencies, conflicts, and registered service names.
4. The broker calls the runtime `Initialize` RPC in dependency order.
   The SDK runs the extension's `Init` then and its `Shutdown` during shutdown.
   A dependency callback socket is available at initialization when the host offered the declared dependency points.
   The SDK connects lazily, and `Depends` supplies generated client adapters.
   A client resolved from that callback may be retained for normal operation,
   but the SDK closes the callback connection before invoking `Shutdown`.

Point calls use generated gRPC server and client wiring.
The generated client adapter presents the same Go interface to the host.
A dead process produces gRPC errors until the daemon restarts.

## Wire contract

The Go interface and message structs are the source of truth.
`mobyextgen` generates the proto, protobuf message code, gRPC service, client and server points, adapters, and conversions without `protoc`, plugins, or other `PATH` tools.
In-process providers implement Go directly; out-of-process providers use gRPC.

The generator supports three method kinds: unary, server-streaming, and client-streaming.
A unary method has the shape `M(context.Context, *Request) (*Reply, error)`.
The generator also accepts the legacy unary shape `M(context.Context, *Request) error`.
A server-streaming method has the shape `M(context.Context, *Request, stream.Sender[Chunk]) error`.
A client-streaming method has the shape `M(context.Context, stream.Receiver[Chunk]) (*Reply, error)`.
The generator rejects bidirectional streams and all other stream signatures.
A stream message type must be a local struct type that is not generic.

### Why not channels?

A Go channel carries values and a close signal.
A channel does not show receive errors, send errors, or the final RPC status.
A channel contract must define rules for these results.
It must also define rules for cancellation, buffers, message ownership, and which side closes the channel.
An adapter between an asynchronous channel and an RPC can need forwarding goroutines and careful cleanup.
The generator can support channels, but channels are not the standard contract.
If channels were the standard contract, each streaming method would include Go-specific concurrency rules.

### Why not iterators?

Iterators are a possible alternative for RPC adapters.
With iterators, a consumer can read values in sequence and stop early without forwarding goroutines.
But an `iter.Seq2[*T, error]` contract must still define rules for errors, cancellation, message ownership, and method completion.
The stream interfaces give the generator a small and explicit contract to send and receive messages.
This contract has a shape similar to the Go I/O interfaces.
`All` and `Copy` give the convenience of iterators, but iterator signatures do not become part of the protocol model.
The cost is two framework interfaces in addition to the standard library types.

### What method scope provides

The enclosing method stays active until the stream is complete.
The return value of the method sets the final success or failure.
Callbacks run synchronously.
Thus, when a transport send blocks, the adapter stops reading from the source.
The adapter does this without a background callback pump or an unlimited buffer.
A normal input EOF closes a client stream, and the adapter then waits for the final reply of the provider.
This EOF alone does not show that the method is successful.
If the provider stops a client stream early, the adapter sees the final status only when the source returns from `Recv`.
Thus, a source that can block must stop when the context of the caller is canceled.
A normal server-streaming EOF alone does not show success.
The final `OK` status of the provider shows success.
Adapters reject a nil stream message from the caller with `InvalidArgument`.
Adapters reject a nil stream message from a provider with `Internal`.

Generated adapters return provider errors without changes.
gRPC converts these errors at the server boundary.
Explicit status errors keep their details.
Context cancellation errors and deadline errors become `Canceled` and `DeadlineExceeded`, also when they are wrapped.
All other errors become `Unknown`.

Messages use proto3.
Supported fields are scalar values, `string`, `[]byte`, repeated scalars such as `[]string`, string-keyed scalar maps such as `map[string]string`, pointer nested messages such as `*Other`, and repeated nested messages such as `[]Other`.
A single nested message must be a pointer; value nested messages are rejected.
Other shapes fail generation rather than producing ambiguous wire code.

Proto3 scalars have no presence: `""`, `0`, and `false` are indistinguishable from unset.
To distinguish "leave unchanged" from an explicit zero, use a message wrapper such as `*Thing`, a separate state flag, or an interpreted `bytes` payload.
Optional scalars, `oneof`, enums, and well-known types such as timestamp and duration are unsupported.
Use a Unix `int64` time, documented string enum-like values, or a `bytes` / JSON payload instead.
There is no typed error schema: Go `error` crosses gRPC as a status, and each point documents its meaning, such as a veto.

The current protocol may renumber, remove, or change fields without migration
handling. Once a contract is declared stable, field numbers and types become
permanent and breaking changes require a new `.vN`.

## Discovery security

Discovery is a root-code-execution boundary.
The daemon scans the extensions directory and launches every accepted
executable, often as root; each accepted binary remains trusted host code.
Its host-attested identity is attribution for lookup and policy, not a sandbox,
permission, or security boundary.

- Directories supplied through `host.WithDirs` are trusted; the framework has no default discovery directory.
  Treat each as a root-owned program directory: only package managers or operators should install files there, and unprivileged users must not be able to write to it.
- A world-writable directory or binary is skipped with a warning.
  A binary or directory owned by anyone other than root or the daemon user is also skipped because its owner could rewrite daemon-executed code.
  Only executable files with valid extension-id names are launched; stray tools and build leftovers are not executed.
- These checks are backstops, not the complete trust model.
  Group-writable binaries are **not** refused; only the world-writable (`o+w`) bit is checked.
  An untrusted group could therefore rewrite a group-writable binary and gain daemon code execution.
  Keep the directory and binaries writable only by root or the daemon user, and do not grant their group to untrusted users.
- Symlink targets are checked as ordinary files.
  Where ownership cannot be determined from file metadata, notably on Windows, the owner check is not enforced; ACL and group policy remain the operator's responsibility.
- One accepted extension that cannot launch, describe, initialize, or coexist with the set fails daemon startup.
  Because every configured directory is scanned at Host construction, a broken or incompatible binary there blocks startup until removed or its directory is no longer configured.
- When running as an unprivileged user, "trusted" means trusted by that user; the world-writable check still applies.
  Packaging should install one non-world-writable binary per extension, named after its id.

There is no sandbox or separate permission model after acceptance.
An extension can do anything the daemon can do.
Administrators control this risk by deciding what to install and inspecting each declaration.

## Design rationale and tradeoffs

Existing systems cover only parts of this model.
Containerd plugins provide versioned names, dependency ordering, and init-time configuration, but remain an in-process registry without process launch, a handshake, generated transports and adapters, socket exposure, or discovery security.
Moby also needs one unit that can expose several points and depend on an out-of-process extension; adapting the registry would split registration from transport and impose different failure and lifecycle rules.
Generic process SDKs such as go-plugin provide a process boundary, gRPC, and a handshake, but not Moby's typed point graph, unified in-process mode, configuration and lifecycle rules, fail-closed loading, or `docker.sock` exposure.
Go's `plugin` and `dlopen` depend on platform, cgo, and exact toolchain or dependency compatibility, cannot be unloaded reliably, and do not suit static daemon builds.
Capability-specific systems such as CSI, CRI, CDI, CNI, NRI, and legacy Moby plugins remain useful in their domains but retain separate declarations, sockets, configuration, and lifecycles instead of one typed dependency graph.
WASM and OPA-style hosts suit sandboxed, reloadable pure computation, not mounts, netlink, subprocesses, existing binaries, or the daemon's full capability surface, though one could implement a point.

As with admission webhooks, each point must explicitly choose fail-open or fail-closed behavior.
Security and veto points generally fail closed; transport or diagnostic points may choose otherwise.
The framework is a standalone Go module, with external-binary discovery and in-process registration on the same extension-facing Go API.
Transport is a host assembly choice: direct Go and gRPC use the same point interface.
Keeping the startup protocol small and the API experimental leaves room for changes after integration experience instead of freezing an unproven public contract.
