//go:build linux

package external

import (
	"context"
	"net"
	"testing"
	"time"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type windowProbeHost struct {
	gen.UnimplementedHostServicesServer
	sent chan int
}

// This counterexample documents why configuring InitialWindowSize on the host
// server does not bound data sent by the host to the child: the child's
// advertised receive window controls that direction of HTTP/2 flow control.
func (s *windowProbeHost) HTTPDo(stream grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame]) error {
	chunk := &gen.BodyChunk{Data: make([]byte, 64<<10)}
	for n := 1; n <= 257; n++ {
		if err := stream.Send(&gen.HTTPFrame{Epoch: 1, CallId: 1, InvocationId: "probe", Payload: &gen.HTTPFrame_Body{Body: chunk}}); err != nil {
			return err
		}
		s.sent <- n
	}
	return nil
}

func TestM6BackpressureReverseHTTPPeerWindowControlsSend(t *testing.T) {
	listener := bufconn.Listen(32 << 20)
	server := grpc.NewServer(grpc.MaxSendMsgSize(84<<10), grpc.MaxRecvMsgSize(84<<10), grpc.InitialWindowSize(84<<10), grpc.InitialConnWindowSize(84<<10))
	probe := &windowProbeHost{sent: make(chan int, 257)}
	gen.RegisterHostServicesServer(server, probe)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///probe", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(32<<20), grpc.WithInitialConnWindowSize(32<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = gen.NewHostServicesClient(conn).HTTPDo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately do not call Recv. A compliant consumer advertises <=84 KiB,
	// but this peer advertises 32 MiB and accepts beyond the M6 16 MiB
	// process queue ceiling despite the host server's 84 KiB setting.
	for n := 1; n <= 257; n++ {
		select {
		case got := <-probe.sent:
			if got != n {
				t.Fatalf("sent frame=%d, want %d", got, n)
			}
		case <-ctx.Done():
			t.Fatalf("probe timed out after %d frames", n-1)
		}
	}
}
