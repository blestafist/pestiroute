//go:build linux

// This test-only child delegates connector behavior to the native Responses
// connector while issuing deliberately mis-scoped HostServices callbacks.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type identity struct {
	Name     string `json:"name"`
	Epoch    uint64 `json:"epoch"`
	ID       string `json:"id"`
	Sequence int    `json:"sequence"`
}

type child struct {
	*external.ConnectorChild
	host                                          gen.HostServicesClient
	identityFile, auditFile, ownName, foreignName string
}

func (c *child) Execute(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	prior, err := readIdentities(c.identityFile)
	if err != nil {
		return err
	}
	sequence := 1
	for _, entry := range prior {
		if entry.Name == c.ownName {
			sequence++
		}
	}
	if err := appendIdentity(c.identityFile, identity{Name: c.ownName, Epoch: first.Epoch, ID: first.InvocationId, Sequence: sequence}); err != nil {
		return err
	}
	if sequence <= 2 {
		foreign, err := waitIdentity(c.identityFile, c.foreignName, sequence)
		if err != nil {
			return err
		}
		if foreign.ID != "" {
			callback := external.NewCredentialClient(c.host, first.Epoch, foreign.ID)
			_, callErr := callback.Get(context.Background(), c.foreignName)
			if callErr == nil {
				audit(c.auditFile, "spoofed-invocation-accepted")
			} else {
				audit(c.auditFile, "spoofed-invocation-denied")
			}
		}
		// A named lookup is still bound to the account selected by this invocation.
		_, callErr := external.NewCredentialClient(c.host, first.Epoch, first.InvocationId).Get(context.Background(), c.foreignName)
		if callErr == nil {
			audit(c.auditFile, "foreign-credential-accepted")
		} else {
			audit(c.auditFile, "foreign-credential-denied")
		}
	}
	if old := findIdentity(prior, c.ownName, sequence-1); old.ID != "" && old.Epoch != first.Epoch {
		callback := external.NewCredentialClient(c.host, old.Epoch, old.ID)
		_, callErr := callback.Get(context.Background(), c.ownName)
		if callErr == nil {
			audit(c.auditFile, "stale-epoch-accepted")
		} else {
			audit(c.auditFile, "stale-epoch-denied")
		}
		if !hasAudit(c.auditFile, "stale-frame-injected") {
			audit(c.auditFile, "stale-frame-injected")
			return c.ConnectorChild.Execute(&executeStream{BidiStreamingServer: stream, first: first, staleEpoch: old.Epoch})
		}
	}
	return c.ConnectorChild.Execute(&executeStream{BidiStreamingServer: stream, first: first})
}

type executeStream struct {
	grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame]
	first      *gen.ExecuteFrame
	staleEpoch uint64
	staleSent  bool
}

func (s *executeStream) Recv() (*gen.ExecuteFrame, error) {
	if s.first != nil {
		first := s.first
		s.first = nil
		return first, nil
	}
	return s.BidiStreamingServer.Recv()
}

func (s *executeStream) Send(frame *gen.ExecuteFrame) error {
	if s.staleEpoch != 0 && !s.staleSent {
		if record := frame.GetRecord(); record != nil && record.RecordType == "HeadFrame" {
			copy := proto.Clone(frame).(*gen.ExecuteFrame)
			copy.Epoch = s.staleEpoch
			s.staleSent = true
			return s.BidiStreamingServer.Send(copy)
		}
	}
	return s.BidiStreamingServer.Send(frame)
}

func main() {
	if len(os.Args) != 5 {
		panic("test child requires identity, audit, own credential, foreign credential")
	}
	connectorSocket, hostSocket := os.Getenv("PESTIROUTE_CONNECTOR_SOCKET"), os.Getenv("PESTIROUTE_HOST_SERVICES_SOCKET")
	ctx := context.Background()
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", hostSocket)
	}
	conn, err := grpc.NewClient("passthrough:///host-services", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxCallSendMsgSize(ipc.MaxGRPCMessageBytes)), grpc.WithContextDialer(dial))
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	host := gen.NewHostServicesClient(conn)
	flow, controlConn, err := external.DialChildFlowControl(ctx, hostSocket, 1)
	if err != nil {
		panic(err)
	}
	defer controlConn.Close()
	base, err := external.NewResponsesChild(host, flow, 1)
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxSendMsgSize(ipc.MaxGRPCMessageBytes))
	gen.RegisterConnectorServer(server, &child{ConnectorChild: base, host: host, identityFile: os.Args[1], auditFile: os.Args[2], ownName: os.Args[3], foreignName: os.Args[4]})
	if err := os.MkdirAll(filepath.Dir(connectorSocket), 0700); err != nil {
		panic(err)
	}
	_ = os.Remove(connectorSocket)
	listener, err := net.Listen("unix", connectorSocket)
	if err != nil {
		panic(err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(connectorSocket) }()
	if err := os.Chmod(filepath.Dir(connectorSocket), 0700); err != nil {
		panic(err)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		fmt.Fprintln(os.Stderr, "test connector stopped")
	}
}

func readIdentities(path string) ([]identity, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []identity
	s := bufio.NewScanner(f)
	for s.Scan() {
		var value identity
		if err := json.Unmarshal(s.Bytes(), &value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, s.Err()
}
func findIdentity(list []identity, name string, sequence int) identity {
	for i := range slices.Backward(list) {
		if list[i].Name == name && list[i].Sequence == sequence {
			return list[i]
		}
	}
	return identity{}
}
func waitIdentity(path, name string, sequence int) (identity, error) {
	for range 500 {
		list, err := readIdentities(path)
		if err != nil {
			return identity{}, err
		}
		if value := findIdentity(list, name, sequence); value.ID != "" {
			return value, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return identity{}, fmt.Errorf("peer invocation identity missing")
}
func appendIdentity(path string, value identity) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(data, '\n'))
	return err
}
func audit(path, record string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintln(file, record)
}
func hasAudit(path, record string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), record)
}
