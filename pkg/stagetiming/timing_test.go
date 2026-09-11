package stagetiming

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/openconfig/gnmi/proto/gnmi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type echoSet struct{ pb.UnimplementedGNMIServer }

func (s *echoSet) Set(_ context.Context, req *pb.SetRequest) (*pb.SetResponse, error) {
	return &pb.SetResponse{Timestamp: time.Now().UnixNano(), Response: []*pb.UpdateResult{{Op: pb.UpdateResult_UPDATE, Path: req.Update[0].Path}}}, nil
}

func TestLocalStageCalibration(t *testing.T) {
	t.Setenv("GNMI_STAGE_TIMING", "1")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts := append(Options(), grpc.Creds(WrapCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}))))
	server := grpc.NewServer(opts...)
	pb.RegisterGNMIServer(server, &echoSet{})
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	go server.Serve(ln)
	defer server.Stop()
	payload := map[string]map[string]string{}
	for i := 0; i < 20000; i++ {
		payload["VnetCalibration|"+net.IPv4(198, 18, byte(i/256), byte(i%256)).String()+"/32"] = map[string]string{"endpoint": "198.19.0.1"}
	}
	data, _ := json.Marshal(payload)
	req := &pb.SetRequest{Update: []*pb.Update{{Path: &pb.Path{Origin: "sonic-db", Elem: []*pb.PathElem{{Name: "CONFIG_DB"}, {Name: "localhost"}, {Name: "VNET_ROUTE_TUNNEL"}}}, Val: &pb.TypedValue{Value: &pb.TypedValue_JsonIetfVal{JsonIetfVal: data}}}}}
	for trial := 0; trial < 5; trial++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := grpc.DialContext(ctx, ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})), grpc.WithBlock())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		client := pb.NewGNMIClient(conn)
		for call := 0; call < 5; call++ {
			ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "x-calibration-id", "loopback"), 10*time.Second)
			_, err = client.Set(ctx, req)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
		}
		conn.Close()
	}
	server.Stop()
	counts := map[string]int{}
	for _, line := range strings.Split(logs.String(), "\n") {
		_, body, ok := strings.Cut(line, "GNMI_STAGE_TIMING ")
		if !ok {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatal(err)
		}
		counts[e.Stage]++
		if !e.OK {
			t.Fatalf("failed stage %+v", e)
		}
	}
	for _, stage := range []string{"protobuf_decode", "unary_handler", "protobuf_encode"} {
		if counts[stage] != 25 {
			t.Fatalf("%s count %d", stage, counts[stage])
		}
	}
	if counts["server_tls_handshake"] != 5 {
		t.Fatal(counts)
	}
	if path := os.Getenv("STAGE_EVIDENCE"); path != "" {
		if err := os.WriteFile(path, logs.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("25 requests of %d JSON bytes; mock handler returns one UpdateResult, not a SONiC backend", len(data))
}
