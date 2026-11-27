// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/extensions"
	greeterv0 "github.com/moby/extensions/example/greeter/v0"
	greeterpb "github.com/moby/extensions/example/greeter/v0/protogen"
	servicev0 "github.com/moby/extensions/extpoints/service/v0"
	"github.com/moby/extensions/grpcproxy"
	"github.com/moby/extensions/host"
	echov1 "github.com/moby/extensions/internal/launcher/echo/v1"
	echopb "github.com/moby/extensions/internal/launcher/echo/v1/protogen"
	"github.com/moby/extensions/testdata/greeter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gotest.tools/v3/assert"
)

// TestPointSocketExposure verifies an out-of-process published Point is
// reachable by name through a proxy.
func TestPointSocketExposure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches a helper binary")
	}

	dir := t.TempDir()
	bin := extensionBinaryPath(dir, greeter.ID)
	build := exec.Command("go", "build", "-o", bin,
		"github.com/moby/extensions/testdata/greeter/cmd/greeter")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build greeter extension: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var policyIdentity extensions.ExtensionIdentity
	var policyPoints []extensions.PointID
	h, err := host.New(ctx,
		host.WithRuntimeDir(shortTempDir(t)),
		host.WithDirs(dir),
		host.WithProviderPolicy(host.PointPolicyFunc(func(identity extensions.ExtensionIdentity, point extensions.PointID) host.PointPolicyResult {
			policyPoints = append(policyPoints, point)
			policyIdentity = identity
			if identity.ID == greeter.ID && point == servicev0.Point.ID() {
				return host.Allow()
			}
			return host.Drop()
		})),
	)
	assert.NilError(t, err)
	defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()
	assert.DeepEqual(t, policyIdentity, extensions.ExtensionIdentity{
		ID: greeter.ID,
		Origin: extensions.ExtensionOrigin{
			Kind:       extensions.ExtensionOriginExecutable,
			Executable: &extensions.ExecutableOrigin{Path: bin},
		},
	})
	assert.DeepEqual(t, policyPoints, []extensions.PointID{servicev0.Point.ID()})

	services := h.PublishedServicesForPoint(greeterv0.Point.ID())
	assert.DeepEqual(t, services, map[extensions.ExtensionID][]string{
		greeter.ID: {"org.mobyproject.extension.example.greeter.v0.Greeter"},
	})
	routes := map[string]grpc.ClientConnInterface{}
	for ext, names := range services {
		conn, ok := h.Conn(ext)
		assert.Check(t, ok, "no connection for extension %q", ext)
		for _, name := range names {
			routes[name] = conn
		}
	}
	assert.Check(t, routes["org.mobyproject.extension.example.greeter.v0.Greeter"] != nil)

	sock := filepath.Join(shortTempDir(t), "api.sock")
	lis, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	proxy := grpcproxy.New(routes)
	go func() { _ = proxy.Serve(lis) }()
	defer proxy.Stop()

	conn, err := grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NilError(t, err)
	defer func() { assert.NilError(t, conn.Close()) }()

	resp, err := greeterpb.NewClient(conn).Greet(ctx, &greeterv0.HelloRequest{Name: "world"})
	assert.NilError(t, err)
	assert.Equal(t, resp.Message, "hello world")
}

// TestProcessOfferIsDroppedByDefault verifies a nil Host policy keeps an
// offered Point private without affecting its internal client wiring.
func TestProcessOfferIsDroppedByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches a helper binary")
	}

	dir := t.TempDir()
	const id = "org.example.exthook.v1"
	bin := extensionBinaryPath(dir, id)
	build := exec.Command("go", "build", "-o", bin, "github.com/moby/extensions/internal/launcher/testdata/exthook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exthook extension: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := host.New(ctx,
		host.WithRuntimeDir(shortTempDir(t)),
		host.WithDirs(dir),
		host.WithClientProviders(echopb.ClientPoint),
	)
	assert.NilError(t, err)
	defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()

	assert.Check(t, h.PublishedServicesForPoint(echov1.Point.ID())[id] == nil)

	conn, ok := h.Conn(id)
	assert.Check(t, ok)
	client := echopb.NewEchoClient(conn)
	resp, err := client.Echo(ctx, &echopb.EchoRequest{Message: "private"})
	assert.NilError(t, err)
	assert.Equal(t, resp.GetMessage(), "private")
}

func TestProcessOfferPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches a helper binary")
	}

	dir := t.TempDir()
	const id = "org.example.exthook.v1"
	bin := extensionBinaryPath(dir, id)
	build := exec.Command("go", "build", "-o", bin, "github.com/moby/extensions/internal/launcher/testdata/exthook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exthook extension: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	t.Run("drop", func(t *testing.T) {
		policyCalls := 0
		h, err := host.New(ctx,
			host.WithRuntimeDir(shortTempDir(t)),
			host.WithDirs(dir),
			host.WithProviderPolicy(host.PointPolicyFunc(func(extensions.ExtensionIdentity, extensions.PointID) host.PointPolicyResult {
				policyCalls++
				return host.Drop()
			})),
		)
		assert.NilError(t, err)
		defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()

		assert.Equal(t, policyCalls, 1)
		assert.Check(t, h.PublishedServicesForPoint(echov1.Point.ID())[id] == nil)
	})

	t.Run("reject", func(t *testing.T) {
		cause := errors.New("publication denied")
		h, err := host.New(ctx,
			host.WithRuntimeDir(shortTempDir(t)),
			host.WithDirs(dir),
			host.WithProviderPolicy(host.PointPolicyFunc(func(extensions.ExtensionIdentity, extensions.PointID) host.PointPolicyResult {
				return host.Reject(cause)
			})),
		)
		assert.Assert(t, h == nil)
		assert.Assert(t, errors.Is(err, cause))
		assert.ErrorContains(t, err, `publish offered points for extension "org.example.exthook.v1"`)
	})
}

// TestInProcessPointExposure verifies a published Point can be collected and
// registered directly on a gRPC server without a process boundary.
func TestInProcessPointExposure(t *testing.T) {
	ctx := t.Context()
	var policyIdentity extensions.ExtensionIdentity
	var policyPoints []extensions.PointID
	h, err := host.New(ctx,
		host.WithRuntimeDir(shortTempDir(t)),
		host.WithExtensions(greeter.Extension),
		host.WithPointServers(greeterpb.ServerPoint),
		host.WithProviderPolicy(host.PointPolicyFunc(func(identity extensions.ExtensionIdentity, point extensions.PointID) host.PointPolicyResult {
			policyPoints = append(policyPoints, point)
			policyIdentity = identity
			if identity.ID == greeter.ID && (point == greeterv0.Point.ID() || point == servicev0.Point.ID()) {
				return host.Allow()
			}
			return host.Drop()
		})),
	)
	assert.NilError(t, err)
	defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()
	assert.DeepEqual(t, policyPoints, []extensions.PointID{greeterv0.Point.ID(), servicev0.Point.ID()})
	assert.Equal(t, policyIdentity, extensions.ExtensionIdentity{
		ID:     greeter.ID,
		Origin: extensions.ExtensionOrigin{Kind: extensions.ExtensionOriginBuiltin},
	})
	_, err = h.Provider(greeterv0.Point.ID(), greeter.ID)
	assert.NilError(t, err)
	assert.DeepEqual(t, h.PublishedServicesForPoint(greeterv0.Point.ID()), map[extensions.ExtensionID][]string{
		greeter.ID: {"org.mobyproject.extension.example.greeter.v0.Greeter"},
	})

	srv := grpc.NewServer()
	h.RegisterInProcessServices(srv)
	sock := filepath.Join(shortTempDir(t), "api.sock")
	lis, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NilError(t, err)
	defer func() { assert.NilError(t, conn.Close()) }()

	resp, err := greeterpb.NewClient(conn).Greet(ctx, &greeterv0.HelloRequest{Name: "world"})
	assert.NilError(t, err)
	assert.Equal(t, resp.Message, "hello world")
}

type multiGreeter struct{}

func (multiGreeter) Greet(_ context.Context, req *greeterv0.HelloRequest) (*greeterv0.HelloReply, error) {
	return &greeterv0.HelloReply{Message: "hello " + req.Name}, nil
}

type multiEcho struct{}

func (multiEcho) Echo(_ context.Context, req *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{Message: req.Message}, nil
}

// TestPublishedServicesCoversAllOfferedPoints verifies an extension offering
// several Points is reported with every published service, not only those of
// a single Point.
func TestPublishedServicesCoversAllOfferedPoints(t *testing.T) {
	ctx := t.Context()

	const id = "org.example.multi.v1"

	ext := extensions.New(extensions.Declaration{
		ID: id,
		Providers: []extensions.Provider{
			greeterv0.Point.Provide(multiGreeter{}),
			echov1.Point.Provide(multiEcho{}),
			servicev0.Offer(greeterv0.Point, echov1.Point),
		},
	})
	h, err := host.New(ctx,
		host.WithRuntimeDir(shortTempDir(t)),
		host.WithExtensions(ext),
		host.WithPointServers(greeterpb.ServerPoint, echopb.ServerPoint),
		host.WithProviderPolicy(host.PointPolicyFunc(func(extensions.ExtensionIdentity, extensions.PointID) host.PointPolicyResult {
			return host.Allow()
		})),
	)

	assert.NilError(t, err)
	defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()

	assert.DeepEqual(t, h.PublishedServices(), map[extensions.ExtensionID][]string{
		id: {
			"moby.extensions.internal.launcher.echo.v1.Echo",
			"org.mobyproject.extension.example.greeter.v0.Greeter",
		},
	})
}

// TestPublishedServicesCoversBuiltinAndProcessExtensions verifies builtin and
// launched extensions are both reported, and only launched ones are reachable
// through a connection.
func TestPublishedServicesCoversBuiltinAndProcessExtensions(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches a helper binary")
	}

	dir := t.TempDir()

	const processID = "org.example.exthook.v1"

	bin := extensionBinaryPath(dir, processID)

	build := exec.Command("go", "build", "-o", bin, "github.com/moby/extensions/internal/launcher/testdata/exthook")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exthook extension: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := host.New(ctx,
		host.WithRuntimeDir(shortTempDir(t)),
		host.WithDirs(dir),
		host.WithExtensions(greeter.Extension),
		host.WithPointServers(greeterpb.ServerPoint),
		host.WithProviderPolicy(host.PointPolicyFunc(func(extensions.ExtensionIdentity, extensions.PointID) host.PointPolicyResult {
			return host.Allow()
		})),
	)

	assert.NilError(t, err)
	defer func() { assert.NilError(t, h.Shutdown(context.WithoutCancel(ctx))) }()

	assert.DeepEqual(t, h.PublishedServices(), map[extensions.ExtensionID][]string{
		greeter.ID: {"org.mobyproject.extension.example.greeter.v0.Greeter"},
		processID:  {"moby.extensions.internal.launcher.echo.v1.Echo"},
	})
	_, ok := h.Conn(processID)
	assert.Check(t, ok)
	_, ok = h.Conn(greeter.ID)
	assert.Check(t, !ok)
}
