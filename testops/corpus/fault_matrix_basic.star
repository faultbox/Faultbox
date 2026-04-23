# testops/corpus/fault_matrix_basic.star
#
# Mock-only corpus spec exercising the v0.3.0 domain-centric builtins:
# fault_assumption, fault_scenario, and fault_matrix. Uses protocol-level
# fault rules (error()) on an HTTP mock so it runs cross-platform — no
# seccomp filter required.
#
# Matrix under test:
#   scenarios: check_health, check_user
#   faults:    health_flaky, users_timeout
#   overrides encode that each fault only affects its matching scenario;
#   unmatched cells fall through to default_expect (normal 200).

api = mock_service("api",
    interface("http", "http", 18099),
    routes = {
        "GET /health":  json_response(status = 200, body = {"status": "ok"}),
        "GET /users/1": json_response(status = 200, body = {"id": "1", "name": "alice"}),
    },
)

# --- Fault assumptions — protocol-level, applied by the HTTP proxy. ---

health_flaky = fault_assumption("health_flaky",
    target = api.http,
    rules = [error(path = "/health", status = 503, message = "service unavailable")],
)

users_timeout = fault_assumption("users_timeout",
    target = api.http,
    rules = [error(path = "/users/*", status = 504, message = "gateway timeout")],
)

# --- Scenarios — each returns the response so expect can inspect it. ---

def check_health():
    return api.http.get(path = "/health")

def check_user():
    return api.http.get(path = "/users/1")

scenario(check_health)
scenario(check_user)

# --- fault_scenario: one scenario + one fault + expect oracle. ---

fault_scenario("health_fails_when_flaky",
    scenario = check_health,
    faults = health_flaky,
    expect = lambda r: assert_eq(r.status, 503),
)

# --- fault_matrix: 2×2 cross-product, per-cell overrides + default. ---

fault_matrix(
    scenarios = [check_health, check_user],
    faults    = [health_flaky, users_timeout],
    overrides = {
        (check_health, health_flaky):  lambda r: assert_eq(r.status, 503),
        (check_user,   users_timeout): lambda r: assert_eq(r.status, 504),
    },
    default_expect = lambda r: assert_eq(r.status, 200),
)
