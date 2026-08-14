# RFC-055 — contract-driven clients.
#
# The same OpenAPI document drives both ends: mock_service() generates the
# callee's routes, client() generates the caller's methods. Nothing in this
# spec spells out a path, an HTTP verb, or a JSON field name — change the
# contract and both sides follow.
#
# Run:
#   faultbox inspect --clients poc/client-rfc055/faultbox.star   # what can I call?
#   faultbox test poc/client-rfc055/faultbox.star

CONTRACT = "./orders.openapi.yaml"

# The callee. Routes come from the contract's declared examples; the
# `/orders/{orderId}` override returns a *degraded* payload — a 200 whose
# eta is null, which the contract declares non-nullable.
orders = mock_service("orders",
    interface("public", "http", 18110),
    openapi = CONTRACT,
    examples = "synthesize",
    overrides = {
        "GET /orders/{orderId}": json_response(
            status = 200,
            body = {"id": 1001, "status": "confirmed", "eta": None},
        ),
    },
)

# Two callers against the same API with different identities. Each gets its
# own swim lane and its own vector-clock participant, so a trace answers
# "which client saw the bad response?" rather than showing one anonymous
# `test` driver.
mobile = client("mobile-app",
    target   = orders.public,
    openapi  = CONTRACT,
    headers  = {"X-Client": "ios/4.2"},
    validate = "response",
)

partner = client("partner-api",
    target   = orders.public,
    openapi  = CONTRACT,
    headers  = {"X-Client": "partner/1.0"},
    validate = "response",
)


def test_contract_violation_is_visible():
    """A 200 that violates the published schema is the finding.

    No assertion had to describe the expected shape — `validate="response"`
    checks the payload against the contract the service publishes, and the
    violation lands in the trace as its own event.
    """
    resp = mobile.get_order(order_id = 1001)

    assert_true(resp.ok, "request should succeed at the transport level")
    assert_eq(resp.status, 200)

    # The service answered 200 but broke its own contract.
    assert_true(
        not resp.contract_ok,
        "expected a contract violation for the null eta, got: " + resp.contract_error,
    )
    assert_true(resp.client == "mobile-app")
    assert_true(resp.operation == "get_order")


def test_both_clients_are_distinct_actors():
    """Two named callers, two lanes, two sets of anchors."""
    mobile.get_order(order_id = 1)
    partner.get_order(order_id = 2)

    mobile_calls = events(where = lambda e: e.type == "client_call" and e.client == "mobile-app")
    partner_calls = events(where = lambda e: e.type == "client_call" and e.client == "partner-api")

    assert_eq(len(mobile_calls), 1)
    assert_eq(len(partner_calls), 1)


def test_unknown_argument_is_caught():
    """The contract knows the parameter names, so a typo can't reach the wire."""
    # `mobile.get_order(order_ids = 1)` would fail at call time with
    # `unknown argument "order_ids" (did you mean "order_id"?)`.
    # Exercised in the Go tests; kept here as documentation of the surface.
    assert_true("get_order" in mobile.operations)
    assert_true("create_order" in mobile.operations)
