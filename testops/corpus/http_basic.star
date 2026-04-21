# testops/corpus/http_basic.star — HTTP mock in isolation.
#
# Exercises only the mock_service HTTP surface: static json_response,
# status_only, and a dynamic handler that echoes a query parameter.
# Catches drift in route matching, body serialization, and header
# defaults independently of the multi-service mock_demo.
#
# Run directly:  faultbox test testops/corpus/http_basic.star

api = mock_service("api",
    interface("http", "http", 18091),
    routes = {
        "GET /status": status_only(204),
        "GET /users/1": json_response(status = 200, body = {
            "id":   "1",
            "name": "alice",
        }),
        "POST /echo": dynamic(lambda req: json_response(status = 200, body = {
            "echoed": req["query"].get("msg", "nothing"),
        })),
    },
)

def test_status_only_returns_configured_code():
    resp = api.http.get(path = "/status")
    assert_eq(resp.status, 204)

def test_json_response_has_seeded_body():
    resp = api.http.get(path = "/users/1")
    assert_eq(resp.status, 200)
    assert_true("alice" in resp.body, "expected seeded name 'alice' in body")

def test_dynamic_handler_echoes_query():
    resp = api.http.post(path = "/echo?msg=hello", body = "{}")
    assert_eq(resp.status, 200)
    assert_true("hello" in resp.body, "expected echoed 'hello' in body")
