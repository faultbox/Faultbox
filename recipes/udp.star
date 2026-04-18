# Faultbox recipes: UDP
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib is embedded in the faultbox binary (@faultbox/ prefix).
#
# Usage:
#     load("@faultbox/recipes/udp.star", "udp")
#
#     dns_broken = fault_assumption("dns_broken",
#         target = dns.main,
#         rules  = [udp.packet_loss(probability = "30%")],
#     )

udp = struct(
    # packet_loss drops a fraction of datagrams. Default is 100% (blackout).
    packet_loss = lambda probability = "100%": drop(probability = probability),

    # dns_flap — aggressive 50% loss typical of unreliable DNS.
    dns_flap = lambda probability = "50%": drop(probability = probability),

    # metrics_slow delays datagrams. Tests StatsD / metrics slow-path.
    metrics_slow = lambda duration = "1s": delay(delay = duration),

    # jitter — fixed per-packet delay for congestion simulation.
    jitter = lambda duration = "100ms": delay(delay = duration),

    # blackhole drops every datagram (total UDP partition).
    blackhole = lambda: drop(),
)
