# Faultbox stdlib: k8s discovery helpers (RFC-036)
#
# Pure string sugar for `service(remote=...)`. No runtime k8s client,
# no kubeconfig parsing, no cluster RPCs — just the standard k8s DNS
# shape so spec authors don't repeat
# `<name>.<namespace>.svc.cluster.local` for every dependency.
#
# Usage:
#     load("@faultbox/discovery/k8s.star", "k8s")
#
#     geo = service("geo-config",
#         interface("public", "http", 8080),
#         remote      = k8s.service("geo-config", namespace = "staging"),
#         healthcheck = http(k8s.endpoint("geo-config", 8080, namespace = "staging") + "/healthz"),
#     )
#
# Cluster connectivity is the user's responsibility — `telepresence connect`,
# `kubectl port-forward`, in-cluster execution, or VPN. See the connectivity
# guide in docs/guides/.

k8s = struct(
    # service(name, namespace = "default") -> "<name>.<namespace>.svc.cluster.local"
    #
    # The standard cluster-DNS form. Faultbox's proxy dials this hostname
    # using whatever resolver is in scope on the host, so the same string
    # works under Telepresence, kubectl port-forward (if you've matched the
    # local hostname), or in-cluster execution.
    service = lambda name, namespace = "default": "%s.%s.svc.cluster.local" % (name, namespace),

    # endpoint(name, port, namespace = "default") -> "<host>:<port>"
    #
    # Convenience for places that need "host:port" — typically inside
    # http()/tcp() healthcheck strings or env values that the SUT consumes
    # raw.
    endpoint = lambda name, port, namespace = "default": "%s.%s.svc.cluster.local:%d" % (name, namespace, port),

    # local(name, port, namespace = "default") -> "<name>.<namespace>:<port>"
    #
    # Short form used inside the cluster (or via Telepresence connect on
    # macOS, where the FQDN suffix is sometimes stripped). Functionally
    # equivalent for in-cluster pods talking to each other; falls back to
    # the resolver's search path.
    local = lambda name, port, namespace = "default": "%s.%s:%d" % (name, namespace, port),
)
