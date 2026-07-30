# Redis with a password — the credential half of the audit.
#
# The service declares its password the way the image takes it. If the step
# never sends AUTH, every command comes back NOAUTH and only an asserted spec
# would notice.

secured = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7-alpine",
    args = ["redis-server", "--requirepass", "faultbox"],
    env = {"REDIS_PASSWORD": "faultbox"},
    healthcheck = ready(timeout = "60s"),
)


def test_authenticated_ping():
    r = secured.main.ping()
    print("PING ok=%s error=%s data=%s" % (r.ok, r.error, r.data))
    assert_true(r.ok, "PING against a password-protected Redis failed: %s" % r.error)
