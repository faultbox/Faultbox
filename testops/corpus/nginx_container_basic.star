# testops/corpus/nginx_container_basic.star
#
# LinuxOnly corpus spec exercising container-mode service() with a real
# Docker image. nginx:alpine is small (~8MB) and quiet on startup, so
# the golden stays portable across CI and Lima.
#
# Coverage:
#   - service() container mode (image=) — pull + start + port mapping
#   - proxy-level HTTP fault injection against a real upstream
#   - healthcheck gating via http(...)
#
# One test per spec by design: running multiple container tests in one
# invocation currently has a state-isolation issue (tracked separately).

web = service("web",
    interface("http", "http", 80),
    image       = "nginx:1.27-alpine",
    healthcheck = http("localhost:80/", timeout = "30s"),
)

def test_proxy_fault_overrides_upstream():
    """error(path='/', status=503) overrides nginx's 200."""
    def scenario():
        resp = web.http.get(path = "/")
        assert_eq(resp.status, 503)
    fault(web.http, error(path = "/", status = 503, message = "maintenance"), run = scenario)
