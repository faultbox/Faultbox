# Faultbox recipes: UDP
#
# Curated failures for UDP services. UDP is datagram-oriented — the only
# per-datagram faults are drop (silent loss) and delay. Corrupt and reorder
# are tracked in RFC-016 as future work.
#
# Usage:
#     load("./recipes/udp.star", "packet_loss", "dns_flap")
#
#     dns_broken = fault_assumption("dns_broken",
#         target = dns.main,
#         rules  = [packet_loss(probability = "30%")],
#     )

# packet_loss drops a fraction of datagrams. Default probability is 100%
# (total blackout).
def packet_loss(probability = "100%"):
    return drop(probability = probability)

# dns_flap simulates intermittent DNS. 50% packet loss is aggressive
# enough that most DNS clients will retry and eventually fail.
def dns_flap(probability = "50%"):
    return drop(probability = probability)

# metrics_slow delays every datagram. Use for StatsD / Datadog metrics
# pipelines to verify the system tolerates slow metric delivery.
def metrics_slow(duration = "1s"):
    return delay(delay = duration)

# jitter delays every packet by a fixed amount. UDP jitter is common on
# congested networks.
def jitter(duration = "100ms"):
    return delay(delay = duration)

# blackhole drops every datagram. Total network partition for UDP traffic.
def blackhole():
    return drop()
