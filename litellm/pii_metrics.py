"""Prometheus metrics for the PII bridge (docs/OBSERVABILITY.md).  No-ops if prometheus_client is
missing.  Served on PII_BRIDGE_METRICS_PORT (own registry; started at most once per process)."""
from __future__ import annotations

import logging
import os
import threading

log = logging.getLogger("pii_bridge.metrics")

try:
    from prometheus_client import CollectorRegistry, Counter, Histogram, start_http_server
except Exception:  # pragma: no cover
    CollectorRegistry = None  # type: ignore

REGISTRY = CollectorRegistry() if CollectorRegistry else None


class _Noop:
    def labels(self, *a, **k):
        return self

    def inc(self, *a, **k): ...
    def observe(self, *a, **k): ...


def _c(name, doc, labels=()):
    return Counter(name, doc, labels, registry=REGISTRY) if REGISTRY else _Noop()


requests_total = _c("pii_requests_total", "Requests handled by the bridge", ("route", "outcome"))
entities_total = _c("pii_entities_total", "Pseudonym handles present in outgoing requests", ("type",))
new_mappings_total = _c("pii_new_mappings_total", "New real->handle mappings created", ("type",))
leak_suspects_total = _c("pii_leak_suspects_total", "Real values found in outgoing bodies after masking", ("route",))
fail_closed_total = _c("pii_fail_closed_total", "Requests refused (fail closed)", ("reason",))
restore_total = _c("pii_restore_total", "Handles seen in responses", ("type", "result"))
audit_errors_total = _c("pii_audit_errors_total", "Audit log write failures")
palena_seconds = (Histogram("pii_palena_seconds", "Palena request-masking latency", registry=REGISTRY,
                            buckets=(.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30))
                  if REGISTRY else _Noop())

_started: int | None = None
_lock = threading.Lock()


def start_server(port: int | None = None) -> bool:
    """Start the exporter once per process. Returns True if (already) serving."""
    global _started
    if REGISTRY is None:
        return False
    if port is None:
        v = os.environ.get("PII_BRIDGE_METRICS_PORT", "")
        if not v.isdigit() or int(v) == 0:
            return False
        port = int(v)
    with _lock:
        if _started is not None:
            return True
        try:
            start_http_server(port, addr=os.environ.get("PII_BRIDGE_METRICS_ADDR", "127.0.0.1"), registry=REGISTRY)
            _started = port
            log.info("pii_bridge: metrics on :%s", port)
        except OSError as e:
            log.error("pii_bridge: cannot start metrics server on :%s: %r", port, e)
            return False
    return True
