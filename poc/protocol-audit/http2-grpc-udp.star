# http2, grpc, udp: the three step protocols the audit could not reach.
#
# The other ten specs here talk to stock images. These three had no server
# to talk to, so they carried unit coverage only — the same gap that let
# the MySQL and Postgres credential bugs survive to v0.16.x: a plugin can
# look correct in isolation and still never complete a round trip.
#
# All three are served by one first-party binary (./servers) built on the
# standard implementations — grpc-go and x/net/http2 — so the bytes on the
# wire are real. Stock images were a poor fit: cleartext h2c is unusual in
# published images, and a gRPC server needs a service definition the client
# also knows.
#
# Build and run (Lima):
#   go build -o /tmp/protocol-servers ./poc/protocol-audit/servers/
#   sudo faultbox test poc/protocol-audit/http2-grpc-udp.star

srv = service("protosrv",
    "/tmp/protocol-servers",
    interface("h2", "http2", 8443),
    interface("rpc", "grpc", 9090),
    interface("dgram", "udp", 9999),
    env = {
        "HTTP2_PORT": "8443",
        "GRPC_PORT": "9090",
        "UDP_PORT": "9999",
    },
    healthcheck = tcp("localhost:8443"),
)


# --- HTTP/2 -----------------------------------------------------------

def test_http2_get():
    r = srv.h2.get(path = "/")
    assert_true(r.ok, "GET / failed: %s" % r.error)
    assert_eq(r.status, 200)


def test_http2_really_spoke_h2():
    """The server echoes the protocol it negotiated.

    A plugin that quietly fell back to HTTP/1.1 would still pass every
    other assertion here, so this is the one that makes the rest mean
    something.
    """
    r = srv.h2.get(path = "/json")
    assert_true(r.ok, "GET /json failed: %s" % r.error)
    assert_eq(r.status, 200)
    print("server reported proto:", r.body)
    assert_true(
        "HTTP/2.0" in r.body,
        "server negotiated %r, not HTTP/2 — the plugin fell back" % r.body,
    )


def test_http2_post_echo():
    r = srv.h2.post(path = "/echo", body = "round-trip")
    assert_true(r.ok, "POST /echo failed: %s" % r.error)
    assert_eq(r.status, 200)
    assert_true(
        "round-trip" in r.body,
        "echo returned %r, not the body sent" % r.body,
    )


def test_http2_surfaces_a_non_2xx():
    """A 503 must arrive as a 503, not as an error or a flattened 200."""
    r = srv.h2.get(path = "/status/503")
    assert_eq(r.status, 503, "non-2xx status was not surfaced")


# --- gRPC -------------------------------------------------------------

def test_grpc_health_check():
    """grpc.health.v1.Health/Check — the one service both sides know.

    Calling it needs no proto shipped with the spec, which is why it is
    the right shape for a protocol-level round-trip check.
    """
    r = srv.rpc.call(method = "/grpc.health.v1.Health/Check")
    assert_true(r.ok, "health Check failed: %s" % r.error)


# --- UDP --------------------------------------------------------------

def test_udp_echo():
    r = srv.dgram.send(data = "ping-udp")
    assert_true(r.ok, "udp send failed: %s" % r.error)
    print("udp reply:", r.data)


def test_udp_send_no_reply_against_a_silent_server():
    """The server swallows "sink" without answering.

    send_no_reply must succeed against a server that genuinely never
    replies — otherwise the step is only ever proven by a timeout, which
    is indistinguishable from a broken send.
    """
    r = srv.dgram.send_no_reply(data = "sink")
    assert_true(r.ok, "send_no_reply failed: %s" % r.error)
