// Package stagetiming supplies opt-in diagnostic timers for local calibration.
// Durations include scheduling/waiting inside each wrapped operation, not CPU time.
package stagetiming

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Enabled is deliberately off unless the local diagnostic environment opts in.
func Enabled() bool { return os.Getenv("GNMI_STAGE_TIMING") == "1" }

type event struct {
	Stage      string `json:"stage"`
	ID         uint64 `json:"id,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	Peer       string `json:"peer,omitempty"`
	DurationNS int64  `json:"duration_ns"`
	Bytes      int    `json:"bytes,omitempty"`
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
}

type sample struct {
	id       uint64
	duration int64
	bytes    int
	callID   string
	peer     string
}

var (
	nextID    atomic.Uint64
	requests  sync.Map
	responses sync.Map
)

func emit(e event) {
	// Log only timing/correlation metadata. Never log payloads, certs or secrets.
	data, _ := json.Marshal(e)
	log.Printf("GNMI_STAGE_TIMING %s", data)
}

type timedCredentials struct {
	credentials.TransportCredentials
}

// WrapCredentials preserves the real handshaker and its authentication policy.
func WrapCredentials(c credentials.TransportCredentials) credentials.TransportCredentials {
	if !Enabled() {
		return c
	}
	return &timedCredentials{c}
}

func (c *timedCredentials) Clone() credentials.TransportCredentials {
	return &timedCredentials{c.TransportCredentials.Clone()}
}

func (c *timedCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	start := time.Now()
	conn, info, err := c.TransportCredentials.ServerHandshake(raw)
	elapsed := time.Since(start).Nanoseconds()
	emit(event{Stage: "server_tls_handshake", Peer: raw.RemoteAddr().String(), DurationNS: elapsed, OK: err == nil})
	return conn, info, err
}

type timedCodec struct{ encoding.Codec }

func isSetRequest(v any) bool {
	t := reflect.TypeOf(v)
	return t != nil && t.Kind() == reflect.Pointer && (t.Elem().Name() == "SetRequest" || t.Elem().Name() == "GetRequest" || t.Elem().Name() == "CapabilityRequest")
}

// Phase measures one real handler operation; nested times are not additive.
func Phase(ctx context.Context, stage string) func() {
	if !Enabled() {
		return func() {}
	}
	start := time.Now()
	return func() {
		elapsed := time.Since(start).Nanoseconds()
		id := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if ids := md.Get("x-calibration-id"); len(ids) == 1 && len(ids[0]) <= 80 {
				id = ids[0]
			}
		}
		emit(event{Stage: stage, CallID: id, DurationNS: elapsed, OK: true})
	}
}

func (c timedCodec) Unmarshal(data []byte, v any) error {
	if !isSetRequest(v) {
		return c.Codec.Unmarshal(data, v)
	}
	start := time.Now()
	err := c.Codec.Unmarshal(data, v)
	s := sample{id: nextID.Add(1), duration: time.Since(start).Nanoseconds(), bytes: len(data)}
	if err != nil {
		emit(event{Stage: "protobuf_decode", ID: s.id, DurationNS: s.duration, Bytes: s.bytes, OK: false})
	} else {
		requests.Store(v, s)
	}
	return err
}

func (c timedCodec) Marshal(v any) ([]byte, error) {
	value, measured := responses.LoadAndDelete(v)
	if !measured {
		return c.Codec.Marshal(v)
	}
	s := value.(sample)
	start := time.Now()
	data, err := c.Codec.Marshal(v)
	elapsed := time.Since(start).Nanoseconds()
	emit(event{Stage: "protobuf_encode", ID: s.id, CallID: s.callID, Peer: s.peer, DurationNS: elapsed, Bytes: len(data), OK: err == nil})
	return data, err
}

func unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	value, measured := requests.LoadAndDelete(req)
	if !measured {
		return handler(ctx, req)
	}
	s := value.(sample)
	if p, ok := peer.FromContext(ctx); ok {
		s.peer = p.Addr.String()
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ids := md.Get("x-calibration-id"); len(ids) == 1 && len(ids[0]) <= 80 {
			s.callID = ids[0]
		}
	}
	start := time.Now()
	resp, err := handler(ctx, req)
	elapsed := time.Since(start).Nanoseconds()
	emit(event{Stage: "protobuf_decode", ID: s.id, CallID: s.callID, Peer: s.peer, DurationNS: s.duration, Bytes: s.bytes, OK: true})
	emit(event{Stage: "unary_handler", ID: s.id, CallID: s.callID, Peer: s.peer, DurationNS: elapsed, OK: err == nil, Status: status.Code(err).String()})
	if err == nil && resp != nil {
		responses.Store(resp, s)
	}
	return resp, err
}

// Options wraps the existing protobuf codec in the pinned grpc-go v1.64.1.
// Put these options before the service's chained interceptors to time that chain.
func Options() []grpc.ServerOption {
	if !Enabled() {
		return nil
	}
	codec := encoding.GetCodec("proto")
	if codec == nil {
		panic("diagnostic timing requires the existing protobuf codec")
	}
	return []grpc.ServerOption{grpc.ForceServerCodec(timedCodec{codec}), grpc.ChainUnaryInterceptor(unary)}
}
