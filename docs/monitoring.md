# Monitoring and load testing

## Monitoring (`/metrics`)

`GET /metrics` on the web port (80) answers in the Prometheus text format, so Prometheus,
Netdata (`go.d` prometheus collector), Telegraf and VictoriaMetrics can scrape it. Names start with
`hdhrmux_`; the process metrics follow the usual `process_*`/`go_*` names.

| Metric | Meaning |
|---|---|
| `hdhrmux_physical_tuners`, `_online`, `_in_use` | tuners in the numbering, reachable, held by the mux |
| `hdhrmux_tuner_online{tuner,global}`, `hdhrmux_tuner_in_use{…}` | the same per physical tuner |
| `hdhrmux_stream_viewers`, `_received_bits_per_second`, `_received_bytes_total`, `_signal_strength_percent`, `_signal_quality_percent`, `_symbol_quality_percent{tuner,channel}` | one per tuned RF channel |
| `hdhrmux_viewers`, `hdhrmux_viewers_sent_bits_per_second`, `hdhrmux_viewer_buffer_max_percent`, `hdhrmux_virtual_tuner_viewers{tuner}` | clients and what they receive |
| `hdhrmux_streams_opened_total`, `_stream_failovers_total`, `_filter_updates_total`, `_idle_streams_reclaimed_total`, `_dropped_batches_total` | counters (dropped batches = slow clients) |
| `hdhrmux_stream_requests_total{code}` | stream requests by answer: `200`, `804`, `805`, `806`, `807`, `404` |
| `hdhrmux_lineup_channels`, `hdhrmux_guide_channels`, `_events`, `_guide_pass_running`, `_scan_running` | lineup, guide and scan state |
| `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, `go_*` | the process |

Useful alerts: `hdhrmux_physical_tuners_online` below `hdhrmux_physical_tuners` (a device is gone),
`rate(hdhrmux_stream_failovers_total[10m]) > 0` (a tuner keeps failing), a rising
`hdhrmux_dropped_batches_total` (a client cannot keep up), and
`rate(hdhrmux_stream_requests_total{code="805"}[10m])` (viewers turned away: out of tuners).

## Load and leak testing (`cmd/loadtest`)

```
loadtest serve                      # multiplexer + simulated devices (3 devices, ATSC 1.0 and 3.0), no hardware
loadtest swarm -viewers 100 -duration 1h -csv run.csv
```

`swarm` starts N viewers that each watch a random channel for a random time before switching (a few
are too slow to keep up), and once a minute reads the multiplexer's `/metrics`: RSS, heap,
goroutines, open file descriptors, tunings, dropped batches, failovers. It finishes with a verdict on
whether any of them kept growing in the second half of the run. `serve -pprof` exposes Go profiling
for a heap profile while the numbers are high.

