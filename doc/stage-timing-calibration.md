# Local server-stage timing experiment

**Draft experiment archive, not production-ready instrumentation.**
Baseline: `8f208e9` (includes the then-local RPC access logging work).
Effective dependencies: grpc-go **1.64.1**, protobuf **1.34.1** through go.mod
replacements. Do not upgrade dependencies and interpret the old measurements as
validation of the upgraded build.

## Activation and boundaries

Set `GNMI_STAGE_TIMING=1` in the diagnostic service environment. Default is off.
Events are emitted with the `GNMI_STAGE_TIMING` prefix as JSON, using monotonic
elapsed nanoseconds. Request payloads, certificates and private keys are not logged.

| Stage | Boundary |
|---|---|
| `server_tls_handshake` | Existing credentials `ServerHandshake`; per connection, includes protocol waiting |
| `protobuf_decode` | Existing protobuf codec `Unmarshal` |
| `unary_handler` | Downstream unary handler/interceptor chain after decode |
| `set_authentication` | Set authentication call, nested in handler |
| `native_backend_set` | `dc.Set` call, nested in handler |
| `protobuf_encode` | Existing protobuf codec `Marshal` for a successful correlated response |

Request/response objects correlate decode, handler and encode events; optional
`x-calibration-id` metadata identifies local trials. TLS uses peer address.
This measures wall duration, not CPU cost, network RTT, or individual TLS phases.
Client benchmark semantics are unchanged; do not subtract these values from its RTT.

## Local validation evidence

- Wrapper smoke: 25 mock 20k-record requests over five mTLS connections; passed
  with race detector using grpc-go 1.64.1. Rechecked directly with the full
  service's prepared vendor tree: `go test -mod=vendor -race -v -count=1
  ./pkg/stagetiming` (Go 1.27.1). This is focused, not full-repository coverage.
- Full diagnostic service built with Go 1.24.4 and SONiC native dependencies.
- Local DUT: `vlab-01`, SONiC `master.0-fe40965`, VS, Force10-S6000 (unchanged),
  4 vCPU / 6 GiB. No shared physical lab was modified.
- After supplying valid temporary VXLAN/VNET prerequisites: **5/5 20k native
  Set calls OK**, zero explicit response errors. No bypass header or validation bypass.
- Original binary, launcher, certificates and ConfigDB digest matched after
  restoration; temporary diagnostic files were removed from the DUT.

| Successful 20k stage | n | Median ms | Observed min–max ms |
|---|---:|---:|---:|
| TLS handshake (separate connections) | 6 | 15.12 | 4.04–20.97 |
| Protobuf decode | 5 | 0.527 | 0.406–6.977 |
| Set authentication | 5 | 0.537 | 0.404–1.890 |
| Native backend | 5 | 15,440.14 | 12,990.38–18,585.73 |
| Unary handler (includes backend) | 5 | 15,451.05 | 12,997.07–18,625.23 |
| SetResponse encode | 5 | 0.00994 | 0.00911–0.01098 |

One Update carries 20k records (1,308,819 JSON bytes); the response acknowledges
one Update and does not echo all route values. Initial failures were caused by a
missing VNET leafref, not proof of inadequate server capacity. Handler/backend
times overlap; min–max is sample variability, not a confidence interval.

Tested binary SHA256:
`6183d9dae1310a2a834cdc6b8b3562c8f0316346fe87bc7a4b316ebf244cca98`.

Local raw evidence (not uploaded by this PR):
`/data/donghaoli/.home-relocated/tmp/opencode/stage-timing-harness/`
contains `dut-results-valid/{calls.json,stages.log,baseline.json,restoration.json}`,
`dut-stage-summary.json` and wrapper test logs. Deployment helpers and transient
credentials are deliberately excluded from the PR.

## Required follow-up before merging

1. Bound correlation state and clean up abandoned responses when encoding never
   runs. Current global object maps are appropriate only for bounded diagnostics.
2. Fix `Phase` event status: its `ok=true` means the timer closure ran, **not**
   operation success. Use `unary_handler.status` and client outcome today.
3. Rename/classify `native_backend_set` for non-native/translib use: this hook is
   on the shared `dc.Set` call. The recorded DUT experiment used only native Set.
4. Quantify instrumentation overhead and strengthen concurrency/error-path tests;
   logging and maps can perturb scheduling. Review interceptor ordering and scope.
5. Port to current upstream using its actual codec API, without silently replacing
   CodecV2 with the old codec or changing authentication semantics.

References: [gRPC credentials](https://pkg.go.dev/google.golang.org/grpc/credentials),
[gRPC encoding](https://pkg.go.dev/google.golang.org/grpc/encoding),
[client performance and queueing](https://grpc.io/docs/guides/performance/).
