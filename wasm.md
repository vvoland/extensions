# Next: hosted WASM extension runtime

## Context

The ownership seam planned previously has landed. `host.Host` now owns
runtime-neutral `loadedExtension` records (`extension` plus an idempotent
`close`), records ownership before `Broker.Register`, and closes resources in
reverse load order on every failure path. Publication state is already separate:
`Host.conns` and `Host.processServices` are populated by the process path
without retaining `*launcher.Launched`.

This plan adds the second runtime that seam anticipated: extensions installed as
WASM modules and hosted in-process. A hosted module remains an external,
separately built and installed artifact, but point calls cross an in-memory
boundary instead of a Unix-socket gRPC hop. The broker-facing
`extensions.Extension` contract is already runtime-neutral, so no exported
extension contract changes.

## Objective

Load `.wasm` modules from the existing extension directories, register them with
the broker exactly like launched processes, and call their point providers
through the existing generated client code. Process extensions, built-ins, the
broker, and all exported host APIs keep their current behavior. New public
surface is limited to the guest SDK package and the documented ABI.

## Decisions

### Runtime: wazero

Embed `github.com/tetratelabs/wazero`. It is pure Go with zero dependencies, so
it preserves cgo-free static builds and the current platform matrix, including
Windows, where the process path cannot even shut down gracefully today.
wasmtime-go requires cgo. The component model is rejected for now: wazero does
not implement it, and Go guests compile to core-module reactors anyway.

One shared `wazero.Runtime` (with its compilation cache) hosts one module
instance per extension.

### Artifact and discovery

A module is a file named `<extension-id>.wasm` directly under an existing
`Options.Dirs` directory. Discovery lives next to `launcher.Binaries` because
that package owns the directory- and file-trust checks: add
`launcher.Modules(ctx, dir)` applying the same untrusted-owner and
world-writable rules, minus the executable bit, and validating the trimmed stem
with `ValidateExtensionID`. `Binaries` must start skipping `*.wasm` files so an
executable-bit module is never launched as a process. Per directory, the host
loads process binaries first, then modules, each in the existing sorted order.
Duplicate IDs across kinds fail at `Broker.Register` as they do today.

### ABI v1

The ABI is a custom core-WASM export/import surface with byte-buffer payloads.
No protocol or `sdk/sdkapi` schema changes: the describe payload reuses the
existing `sdkapi.Declaration` message, so launcher-side validation and
conversion (`declaredDependencies`, `declaredConflicts`, `declaredServices`,
`validateDeclaredServices`, the ID-equals-filename rule) is extracted into a
shared internal helper instead of duplicated.

Guest exports:

- `moby_abi_version() -> u32`, must return `1`;
- `moby_alloc(size u32) -> u32` and `moby_free(ptr u32)`, guest-owned buffers;
- `moby_describe() -> u64`, a packed result whose payload is `sdkapi.Declaration`
  proto bytes;
- `moby_initialize(config_ptr, config_len u32) -> u64`, config as the JSON
  encoding of `extensions.Config`;
- `moby_invoke(method_ptr, method_len, req_ptr, req_len u32) -> u64`, keyed by
  the full gRPC method name with a proto-encoded request payload;
- `moby_shutdown() -> u64`.

A packed `u64` is `(ptr << 32) | len` of a guest buffer the host copies and then
frees with `moby_free`. Every result buffer is framed as a little-endian `u32`
gRPC status code followed by the response payload on code `0` or a UTF-8 error
message otherwise, so guest errors map onto the same status space the process
path uses.

Host imports (module `moby_ext_v1`) carry dependency callbacks:

- `callback_invoke(method_ptr, method_len, req_ptr, req_len u32) -> u64`
  returning `(handle << 32) | len`;
- `callback_take(handle u32, dst_ptr u32)` copying the response into a buffer
  the guest allocated for that length and releasing the handle.

The two-phase callback shape exists so the host never re-enters guest exports
(such as `moby_alloc`) while a guest call is on the stack, which sidesteps
wasmexport re-entrancy questions entirely.

WASI is instantiated only to satisfy the Go runtime: stdout and stderr route to
the host logger (parity with process stderr piping), no preopened directories,
no environment, no network. Clocks and randomness remain available.

### Host-side call path

`internal/wasm` provides a conn type implementing `grpc.ClientConnInterface`
whose `Invoke` proto-marshals the request, calls `moby_invoke`, and unmarshals
the framed response; `NewStream` returns Unimplemented until Stage 4. Because
`clientpoint.Provider` is `func(grpc.ClientConnInterface) extensions.Provider`,
every generated `ClientProvider` and `clientpoint.Registration` works unchanged,
and `mobyextgen` needs no edits.

`internal/wasm.Load(ctx, path, ...)` mirrors `launcher.Launch`: instantiate,
check `moby_abi_version`, call `moby_describe`, validate the declaration against
the filename stem, and return a `Loaded` mirroring `launcher.Launched` (`ID`,
`Dependencies`, `Conflicts`, `Points`, `ProviderServices`, `Conn`, `Initialize`,
`Close`). `Close` closes the module instance and is idempotent via the same
`sync.Once`-with-retained-error pattern as `processShutdown.Close`.

### Lifecycle split

Unlike the process path, where `Declaration.Shutdown` and the resource close
alias one `Launched.Close`, a module gets the distinct pair the seam was built
for: `Declaration.Init` calls `moby_initialize` with the broker-selected config,
`Declaration.Shutdown` calls `moby_shutdown` under broker reverse-dependency
ordering only after successful initialization, and `loadedExtension.close`
unconditionally releases the instance. Config therefore arrives at Init time,
matching in-process semantics; the startup-envelope timing remains a
process-runtime constraint only.

### Concurrency and cancellation

A module instance is not safe for concurrent calls (Go wasip1 guests are
single-threaded), so all guest entries serialize on a per-instance mutex.
Cross-instance callback chains follow the broker's acyclic dependency graph, so
the per-instance locks cannot deadlock. Enable wazero's close-on-context-done:
lifecycle calls (`describe`, `initialize`, `shutdown`) run under deadlines and a
hung guest is terminated and reported as that step's failure, while point
invokes run under `context.WithoutCancel` so a routine caller cancellation
cannot destroy the shared instance. Both limits are documented; instance
pooling and call cancellation are future work.

### Guest SDK

New package `sdk/wasm` (build-constrained to wasip1) is the module counterpart
of `sdk.Main`. Guests build with `GOOS=wasip1 GOARCH=wasm go build
-buildmode=c-shared`, producing a reactor whose `_initialize` runs package
initializers but not `main`, so registration must complete during package
initialization; Stage 1 validates this wiring. The SDK owns the
`//go:wasmexport` entry points and buffer bookkeeping, and reuses the existing
registration surfaces:

- `Register(ext extensions.Extension, points ...serverpoint.Registration)`
  builds the dispatch table by collecting `grpc.ServiceDesc`s through a
  `grpc.ServiceRegistrar` and serving `moby_invoke` through each
  `MethodDesc.Handler` with a byte-decoding `dec`;
- `Depends(regs ...clientpoint.Registration)` builds the Init-time resolver over
  a `grpc.ClientConnInterface` backed by `callback_invoke`/`callback_take`.

The method-table-plus-unary-dispatch mechanism is shared with the host's
callback side in a small internal helper, since Stage 3 replaces nothing: the
gRPC callback server on `callback.sock` stays for processes, and the same
`Options.DependencyProviders` registrations (including the exactly-one-provider
ambiguity rule from `serveCallback`) additionally serve module callbacks
in-memory.

## Independently reviewable stages

### Stage 1: runtime package and ABI

Add the wazero dependency and `internal/wasm`: runtime construction, `Load`,
`Loaded`, the conn's `Invoke`, framing, per-instance serialization, deadline
policy, and idempotent `Close`. Extract the shared declaration
validation/conversion helper from `internal/launcher` first as its own commit.
Test against a raw testdata guest that hand-rolls `//go:wasmexport` and framing
without the SDK, built by the tests with the same `exec.Command("go", "build")`
pattern the host tests already use. This stage also proves the reactor-build and
wasmexport assumptions before anything depends on them.

### Stage 2: host integration

Wire modules into `host.New`: `launcher.Modules` discovery plus the `Binaries`
`.wasm` exclusion; refactor `extensionFromLaunched` into a runtime-neutral
adapter over a private handshake record (id, dependencies, conflicts, points,
conn, init, shutdown) with thin process and module mappings; add a `loadModule`
helper implementing the same guarded transaction as `loadProcess`; record the
`loadedExtension` before `Broker.Register`. Reject module declarations carrying
`provider_services` with a clear error until Stage 4, and do not add modules to
`Host.conns` or `Host.processServices` yet. End-to-end host tests drive a
module's point through a generated `ClientPoint`, and reuse the seam's failure
matrix: adaptation error, register error, partial `Init` error, and normal
shutdown all release the instance in reverse load order.

### Stage 3: guest SDK and dependency callbacks

Add `sdk/wasm`, the shared unary-dispatch helper, and the `moby_ext_v1` host
imports routed to `Options.DependencyProviders`. Rewrite the testdata guest on
the SDK and cover: init-order config delivery, guest `Shutdown` running under
broker ordering only after initialization, a guest resolving a declared
dependency during Init (including one whose provider is another module), and
callback errors surfacing as Init failures.

### Stage 4 (gated): socket service publication

Only if module-published services are actually wanted on the socket: implement
`NewStream` unary emulation on the module conn honoring the
`grpc.ForceCodecV2` passthrough codec `grpcproxy` sends, generalize
`Host.processServices` into a runtime-neutral publication index, include module
conns in `Host.conns`, lift the Stage 2 `provider_services` rejection, and prove
it through the existing proxy tests. Streaming methods stay unsupported and must
fail with Unimplemented. Do not start this stage without confirming the need;
Stages 1–3 are complete and useful without it.

## Files and symbols

- `go.mod`: add `github.com/tetratelabs/wazero`.
- `internal/wasm` (new): runtime, `Load`, `Loaded`, conn, framing, dispatch of
  lifecycle exports.
- `internal/launcher/launcher.go`: add `Modules`, exclude `.wasm` from
  `Binaries`, and extract shared declaration validation; `Launch`, `Launched`,
  and `processShutdown` behavior otherwise unchanged.
- `host/host.go`: `loadModule`, the runtime-neutral handshake adapter replacing
  `extensionFromLaunched`'s direct `*launcher.Launched` parameter, module wiring
  in `New`; `loadedExtension`, reverse-close helpers, `Conn`, and
  `ServicesForPoint` unchanged until Stage 4.
- `sdk/wasm` (new): guest exports, `Register`, `Depends`, callback-backed
  resolver.
- `sdk/sdkapi`: reused as the describe payload; no schema change.
- `core.go`, `internal/broker/broker.go`, `clientpoint`, `serverpoint`,
  `cmd/mobyextgen`, generated `protogen` code: unchanged.
- `docs/DESIGN.md` (or a new `docs/WASM.md`): document ABI v1, the trust and
  WASI sandbox posture, and the concurrency/cancellation limits.
- Tests: `internal/wasm` unit tests with the raw fixture; `host` end-to-end
  tests with SDK-based testdata modules; existing launcher, broker, core, and
  socket tests keep their expectations.

## Acceptance criteria

- A `.wasm` module in an extension directory registers, initializes in
  dependency order with its selected config, serves point calls through
  unchanged generated client code, shuts down under broker ordering, and has
  its instance closed unconditionally afterward.
- Every load, register, and partial-init failure releases the instance, in
  reverse load order alongside process resources, preserving the seam's
  no-leak matrix.
- `moby_shutdown` runs only for initialized modules; `Loaded.Close` is
  idempotent and retains its first error.
- Guest errors carry gRPC status codes end to end through the framed ABI.
- Concurrent point calls to one module serialize without deadlock, including
  module-to-module dependency callbacks.
- Process extensions, built-ins, provider precedence, dependency ordering,
  conflict handling, callback ambiguity rules, `Host.Conn`,
  `Host.ServicesForPoint`, and expose-only behavior keep passing their existing
  tests unchanged through Stage 3.
- A world-writable or untrusted-owner module file or directory is skipped
  exactly like a process binary.
- CI passes with the wasip1 fixture builds on all current platforms.

## Explicit non-goals

- Streaming point methods over the module conn (unary only).
- Instance pooling, concurrent guest execution, or point-call cancellation.
- CPU metering, memory-limit policy, or fuel; wazero defaults apply for now.
- Filesystem, network, or environment access for guests.
- TinyGo or non-Go guest support guarantees (the ABI does not preclude them).
- Component model, WIT, or module signing.
- Reload, health checking, restart, or supervision.
- Any change to the process startup envelope, `sdk/sdkapi` protocol, exported
  `core.go` contracts, or broker semantics.
