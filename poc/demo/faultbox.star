# faultbox.star — Demo Day: Order + Inventory system
#
# order-svc (HTTP) → inventory-svc (TCP + WAL)
#
# Run all:   faultbox test faultbox.star
# Run one:   faultbox test faultbox.star --test happy_path
# Trace:     faultbox test faultbox.star --output trace.json --shiviz trace.shiviz

# --- Topology ---

inventory = service("inventory",
    "/tmp/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders",
    "/tmp/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

# --- Tests ---

def test_happy_path():
    """Place an order — stock reserved, WAL written."""
    # Check stock is available.
    resp = orders.get(path="/inventory/widget")
    assert_eq(resp.status, 200)
    assert_true("100" in resp.body, "expected 100 widgets in stock")

    # Place order.
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
    assert_true("confirmed" in resp.body, "expected confirmed order")

    # Verify stock decreased.
    resp = orders.get(path="/inventory/widget")
    assert_eq(resp.status, 200)
    assert_true("99" in resp.body, "expected 99 widgets after reservation")

    # Temporal: WAL must have been opened for writing.
    assert_eventually(
        service = "inventory",
        syscall = "openat",
        path = "/tmp/inventory.wal",
    )

def test_inventory_slow():
    """Inventory writes delayed 500ms — order still succeeds but slow."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"gadget","qty":1}')
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 400, "expected delay > 400ms")
        assert_true("confirmed" in resp.body)

        # WAL was opened despite delay.
        assert_eventually(
            service = "inventory",
            syscall = "openat",
            path = "/tmp/inventory.wal",
        )
    fault(inventory, write=delay("500ms"), run=scenario)

def test_inventory_unreachable():
    """Order service can't connect to inventory — returns 503."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
        assert_true("unreachable" in resp.body, "expected unreachable error")

        # No WAL open should occur (inventory never reached).
        assert_never(
            service = "inventory",
            syscall = "openat",
            path = "/tmp/inventory.wal",
        )
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)

def test_wal_fsync_failure():
    """WAL fsync fails — reservation should fail, data integrity preserved."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"gizmo","qty":1}')
        # Order should fail because inventory can't persist.
        assert_true(resp.status != 200, "expected non-200 on fsync failure")

        # fsync was attempted but denied.
        assert_eventually(
            service = "inventory",
            syscall = "fsync",
            decision = "deny*",
        )
    fault(inventory, fsync=deny("EIO"), run=scenario)

def test_disk_full():
    """WAL write fails with ENOSPC — reservation should fail."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status != 200, "expected failure on disk full")
    fault(inventory, write=deny("ENOSPC"), run=scenario)

def test_flaky_network():
    """20% of connections fail — order should still succeed (retry-safe).
    Run with: faultbox test faultbox.star --test flaky_network --runs 100 --fail-only
    """
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        # With 20% failure, most runs should succeed (order-svc retries or fails).
        # This test explores the space — failures are interesting counterexamples.
        assert_true(resp.status == 200 or resp.status == 503,
            "expected 200 or 503, got " + str(resp.status))
    fault(orders, connect=deny("ECONNREFUSED", probability="20%"), run=scenario)
