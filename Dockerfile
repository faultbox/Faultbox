FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY faultbox faultbox-shim /usr/local/bin/

ENTRYPOINT ["faultbox"]
