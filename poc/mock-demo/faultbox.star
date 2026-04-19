# poc/mock-demo/faultbox.star
#
# End-to-end example showing the v0.8 mock service stack: HTTP auth stub
# (the JWKS use case), Redis cache with seeded state, Kafka broker for
# event publishing, and a gRPC feature-flag service. The "system under
# test" — `api` — would normally be a real container; here we focus on
# the mocks themselves and exercise them with step calls so the spec
# runs without a real backend.
#
# Run:
#   faultbox test poc/mock-demo/faultbox.star

load("@faultbox/mocks/kafka.star",   "kafka")
load("@faultbox/mocks/redis.star",   "redis")
load("@faultbox/mocks/mongodb.star", "mongo")

# --- Auth stub: JWKS + token endpoints over HTTP ---
#
# Real OIDC issuers serve a JWKS at /.well-known/openid-configuration/jwks
# and accept token requests at /token. The dynamic handler reflects the
# requested user back as the subject — useful for tests that vary the
# logged-in user across cases.

auth_stub = mock_service("auth",
    interface("http", "http", 18090),
    routes = {
        "GET /.well-known/openid-configuration": json_response(status = 200, body = {
            "issuer":   "http://auth:18090",
            "jwks_uri": "http://auth:18090/.well-known/openid-configuration/jwks",
        }),
        "GET /.well-known/openid-configuration/jwks": json_response(status = 200, body = {
            "keys": [{
                "kty": "OKP", "crv": "Ed25519",
                "kid": "test-1",
                "x":   "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo",
            }],
        }),
        "POST /token": dynamic(lambda req: json_response(status = 200, body = {
            "access_token": "stub-jwt-for-" + req["query"].get("user", "anonymous"),
            "token_type":   "Bearer",
            "expires_in":   3600,
        })),
        "GET /health": status_only(204),
    },
)

# --- Feature flags over gRPC ---
#
# Reflective Go/Node clients decode google.protobuf.Struct naturally;
# typed clients that need a specific message type would need the real
# backend.

flags_stub = mock_service("flags",
    interface("main", "grpc", 50051),
    routes = {
        "/flags.v1.Flags/Get":  grpc_response(body = {"enabled": True, "variant": "B"}),
        "/flags.v1.Flags/List": grpc_response(body = {
            "flags": [
                {"name": "rollout",  "enabled": True},
                {"name": "darkmode", "enabled": False},
            ],
        }),
        "/flags.v1.Flags/Fail": grpc_error(code = "UNAVAILABLE", message = "backend down"),
    },
)

# --- Redis cache: seeded state ---

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 16379),
    state = {
        "config:max_retries": "3",
        "config:timeout_ms":  "5000",
        "flag:new_ui":        "true",
    },
)

# --- Kafka broker: empty topics ready for produce/consume ---

bus = kafka.broker(
    name      = "bus",
    interface = interface("main", "kafka", 19092),
    topics    = {"orders": [], "payments": []},
)

# --- MongoDB: seeded users collection ---

users_db = mongo.server(
    name      = "users-stub",
    interface = interface("main", "mongodb", 27027),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob",   "role": "user"},
        ],
    },
)

# --- Tests ---

def test_jwks_endpoint():
    """Auth stub serves the OIDC JWKS document."""
    resp = auth_stub.http.get(path = "/.well-known/openid-configuration/jwks")
    assert_eq(resp.status, 200)
    assert_true("test-1" in resp.body, "expected kid 'test-1' in JWKS body")

def test_token_endpoint_dynamic():
    """Dynamic handler reflects the user query parameter back as subject."""
    resp = auth_stub.http.post(path = "/token?user=alice", body = "{}")
    assert_eq(resp.status, 200)
    assert_true("stub-jwt-for-alice" in resp.body, "expected stub JWT for alice")

def test_health_status_only():
    """status_only(204) returns the configured code with no body."""
    resp = auth_stub.http.get(path = "/health")
    assert_eq(resp.status, 204)

def test_kafka_broker_responds():
    """Real kafka publish reaches the kfake-backed broker."""
    bus.main.publish(topic = "orders", key = "o-1", value = '{"id":1}')
    # publish() returns success when the broker accepts; assertion is the
    # absence of a panic. Consumers in real specs would verify via
    # consume() or assert_eventually(events()...).

def test_redis_seeded_state():
    """GET on a seeded key returns the seeded value."""
    resp = cache.main.get(key = "flag:new_ui")
    assert_eq(resp.status, 0)
    assert_true("true" in resp.body, "expected seeded value 'true' for flag:new_ui")

def test_mongo_handshake_and_find():
    """MongoDB driver completes handshake and reads seeded docs."""
    resp = users_db.main.find(collection = "users", filter = {})
    assert_eq(resp.status, 0)
    # resp.data is a list of seeded documents.
