// Package ipc contains the bounded, transport-independent M6 wire codecs.
package ipc

import (
	"errors"
	"fmt"
	"io"
	"math"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/protobuf/proto"
)

const (
	MaxFragmentBytes  = 64 << 10
	MaxWireFrameBytes = 84 << 10
	grpcPrefixBytes   = 5
	// MaxGRPCMessageBytes excludes gRPC's five-byte per-message prefix.
	MaxGRPCMessageBytes = MaxWireFrameBytes - grpcPrefixBytes
	maxMessageBytes     = MaxGRPCMessageBytes
)

var ErrMalformed = errors.New("malformed IPC record")

// ValidateSerializedMessage applies the gRPC message limit; the transport's
// five-byte prefix is additional and makes the complete wire frame <=84 KiB.
func ValidateSerializedMessage(data []byte) error {
	if len(data) > maxMessageBytes {
		return ErrMalformed
	}
	return nil
}

type Identity struct {
	Epoch, CallID, RecordID uint64
	Type                    string
}

// Fragment serializes one entire typed protobuf value and emits its uniform
// RecordFragment stream. Only one fragment is materialized at a time.
func WriteRecord(id Identity, value proto.Message, send func(*gen.RecordFragment) error) error {
	if value == nil || id.Type == "" {
		return fmt.Errorf("%w: missing value/type", ErrMalformed)
	}
	if string(value.ProtoReflect().Descriptor().Name()) != id.Type {
		return fmt.Errorf("%w: record type mismatch", ErrMalformed)
	}
	if id.Type == "BodyChunk" {
		return fmt.Errorf("%w: body chunks bypass typed records", ErrMalformed)
	}
	if send == nil {
		return fmt.Errorf("%w: missing fragment writer", ErrMalformed)
	}
	b, err := proto.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return send(&gen.RecordFragment{Epoch: id.Epoch, CallId: id.CallID, RecordId: id.RecordID, RecordType: id.Type, Final: true})
	}
	for off, seq := 0, uint64(0); off < len(b); seq++ {
		end := off + MaxFragmentBytes
		if end > len(b) {
			end = len(b)
		}
		part := append([]byte(nil), b[off:end]...)
		f := &gen.RecordFragment{Epoch: id.Epoch, CallId: id.CallID, RecordId: id.RecordID, RecordType: id.Type, TotalLength: uint64(len(b)), Offset: uint64(off), Sequence: seq, Final: end == len(b), Data: part}
		if proto.Size(f) > maxMessageBytes {
			return fmt.Errorf("%w: encoded fragment exceeds frame bound", ErrMalformed)
		}
		if err := send(f); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// Reassemble validates a complete record before decoding it. It never
// allocates according to the peer-declared total length.
func Reassemble(id Identity, fragments []*gen.RecordFragment, dst proto.Message) error {
	if len(fragments) == 0 || dst == nil {
		return ErrMalformed
	}
	a := Assembler{identity: id}
	for _, f := range fragments {
		if err := a.Add(f); err != nil {
			return err
		}
	}
	return a.Decode(dst)
}

// Assembler accepts one record incrementally and holds no partial decoded value.
type Assembler struct {
	identity          Identity
	data              []byte
	total             uint64
	next              uint64
	started, complete bool
}

// NewAssembler constructs an incremental receiver for one typed record.
func NewAssembler(id Identity) *Assembler { return &Assembler{identity: id} }

// Complete reports whether the final fragment for this record was accepted.
func (a *Assembler) Complete() bool { return a.complete }

func (a *Assembler) Add(f *gen.RecordFragment) error {
	if a.complete || f == nil || a.identity.Type == "BodyChunk" {
		return ErrMalformed
	}
	if proto.Size(f) > maxMessageBytes || len(f.Data) > MaxFragmentBytes || f.Epoch != a.identity.Epoch || f.CallId != a.identity.CallID || f.RecordId != a.identity.RecordID || f.RecordType != a.identity.Type || f.Sequence != a.next || f.Offset != uint64(len(a.data)) {
		return ErrMalformed
	}
	if f.TotalLength > uint64(^uint(0)>>1) {
		return ErrMalformed
	}
	if !a.started {
		a.total = f.TotalLength
		a.started = true
	} else if f.TotalLength != a.total {
		return ErrMalformed
	}
	if uint64(len(f.Data)) > math.MaxUint64-uint64(len(a.data)) || a.total < uint64(len(a.data))+uint64(len(f.Data)) {
		return ErrMalformed
	}
	if f.Final != (uint64(len(a.data))+uint64(len(f.Data)) == a.total) {
		return ErrMalformed
	}
	if a.total == 0 && !(a.next == 0 && f.Final && len(f.Data) == 0) {
		return ErrMalformed
	}
	a.data = append(a.data, f.Data...)
	a.next++
	a.complete = f.Final
	return nil
}

func (a *Assembler) Decode(dst proto.Message) error {
	if !a.complete || dst == nil || uint64(len(a.data)) != a.total {
		return ErrMalformed
	}
	if string(dst.ProtoReflect().Descriptor().Name()) != a.identity.Type {
		return ErrMalformed
	}
	tmp := dst.ProtoReflect().Type().New().Interface()
	if err := proto.Unmarshal(a.data, tmp); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	proto.Reset(dst)
	proto.Merge(dst, tmp)
	return nil
}

// WriteBody emits opaque chunks incrementally without retaining a chunk list.
func WriteBody(body []byte, routeLimit uint64, send func([]byte) error) error {
	if send == nil || uint64(len(body)) > routeLimit {
		return ErrMalformed
	}
	for off := 0; off < len(body); {
		end := off + MaxFragmentBytes
		if end > len(body) {
			end = len(body)
		}
		if err := send(append([]byte(nil), body[off:end]...)); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// WriteBodyReader forwards stream/callback bodies in bounded chunks without
// accumulating the full HTTP or user-facing response body.
func WriteBodyReader(r io.Reader, send func([]byte) error) error {
	if send == nil {
		return ErrMalformed
	}
	if r == nil {
		return nil
	}
	buf := make([]byte, MaxFragmentBytes)
	emptyReads := 0
	for {
		n, err := r.Read(buf)
		if n < 0 || n > len(buf) {
			return ErrMalformed
		}
		if n > 0 {
			emptyReads = 0
			if sendErr := send(append([]byte(nil), buf[:n]...)); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			emptyReads++
			if emptyReads == 100 {
				return io.ErrNoProgress
			}
		}
	}
}

// BodyAssembler consumes route-bounded Execute/EstimateUsage request chunks as
// they arrive. HTTP and response bodies stay incremental and must not use it.
type BodyAssembler struct {
	routeLimit uint64
	body       []byte
	complete   bool
}

func (a *BodyAssembler) Add(chunk []byte) error {
	if a.complete || len(chunk) == 0 || len(chunk) > MaxFragmentBytes || uint64(len(a.body)) > a.routeLimit || uint64(len(chunk)) > a.routeLimit-uint64(len(a.body)) {
		return ErrMalformed
	}
	a.body = append(a.body, chunk...)
	return nil
}
func (a *BodyAssembler) Finish() ([]byte, error) {
	if a.complete {
		return nil, ErrMalformed
	}
	a.complete = true
	return a.body, nil
}
