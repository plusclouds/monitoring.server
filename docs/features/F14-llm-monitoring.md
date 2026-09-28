# F14: LLM monitoring — infrastructure, tracing and quality

**Status:** Draft · **Phase:** phase 2 (LLM infrastructure, Prometheus scraping, trace metrics); phase 3 (content capture, quality evaluation) · **Related:** [F03](F03-plugin-sdk.md), [F05](F05-state-and-incidents.md), [F07](F07-metrics-store-and-retention.md), [F08](F08-push-ingestion.md), [F13](F13-usage-metering.md)

## Summary

Language models are now part of the infrastructure customers run and buy. The engine watches them at two levels:

1. **LLM infrastructure:** is the model endpoint up, how fast does it answer, and how loaded are its servers and GPUs. This works like any other check or collector.
2. **LLM quality and application behavior:** what applications actually send and receive, what it costs, and whether answers are good. This needs trace ingestion and evaluators, which the engine did not have before.

Both levels end in the same place as everything else: metrics, thresholds, incidents and signed webhooks. A drop in answer quality opens an incident the same way a failed fan does.

General application performance monitoring (tracing of arbitrary services) stays out of scope. Only LLM calls and the application steps around them (retrieval, tool calls, agent steps) are traced.

## Scope

| Phase 2 | Phase 3 | Later |
| --- | --- | --- |
| `llm.inference` synthetic check | Prompt and response capture (opt-in per tenant) with redaction | Drift and anomaly detection on quality scores (with the phase 3 baseline alerts) |
| `prometheus.scrape` collector (vLLM, TGI, Ollama exporters, NVIDIA DCGM, any Prometheus endpoint) | Evaluators: deterministic rules and LLM-as-judge | Trace storage on ClickHouse |
| OTLP trace ingestion, metadata only, turned into metrics | `llm.eval` golden-dataset check | Prompt and model version comparison reports |
| Model price table and cost metrics | User feedback API | |
| Templates and Grafana dashboards for LLM endpoints and applications | Trace search API and Grafana trace panels | |

## Level 1: LLM infrastructure

### Devices

| Type | What it is | Typical checks |
| --- | --- | --- |
| `llm_endpoint` | An OpenAI-compatible inference endpoint: self-hosted (vLLM, TGI, Ollama, LiteLLM gateway) or hosted (OpenAI, Anthropic, Azure OpenAI, others) | `llm.inference`, `tls`, `prometheus.scrape` for self-hosted servers |
| `gpu_server` | A server whose GPUs run inference | `prometheus.scrape` (DCGM exporter), `redfish.health`, `icmp` |

A self-hosted endpoint depends on the servers that run it ([ADR-0014](../adr/0014-device-model-containment-dependencies-sites.md)), so a GPU server failure suppresses the endpoint's incidents under it.

### `llm.inference` (check)

Sends a small fixed request and measures the answer.

- Config: API style (`openai-chat`, `anthropic-messages`, `ollama`), model name, prompt (default: a one-line prompt with a known answer), `max_output_tokens` (default 16, maximum 256), optional streaming, optional expected-answer match (contains, regex, JSON schema).
- Metrics: `ttft_ms` (time to first token, with streaming), `total_ms`, `output_tokens_per_second`, `input_tokens`, `output_tokens`, `status_code`.
- Status: CRITICAL on connection failure, 5xx or timeout; WARNING on rate limiting (`429`) or an expected-answer mismatch.
- Credential: `http_bearer` or a new `llm_api_key` type with the provider's header name.
- Every run costs tokens on paid APIs, so the minimum interval is 60 s and the manifest's `MinInterval` enforces it. Token usage of the check itself is reported as a metric so customers can see what monitoring costs them.

### `prometheus.scrape` (collector)

Reads a Prometheus text or OpenMetrics endpoint and stores the selected series.

- Config: URL, optional credential, metric allowlist (names or regex), label mapping to objects (for example one object per `gpu` label), and a series cap per check (default 500).
- Built-in profiles select the useful series for vLLM (running and waiting requests, KV-cache usage, time to first token and inter-token latency histograms, token throughput), TGI, Ollama and NVIDIA DCGM (GPU utilization, memory used, temperature, power, XID errors, ECC errors).
- The collector is general: it also covers any other service that exposes Prometheus metrics.

## Level 2: LLM quality and application behavior

### Ingestion: OpenTelemetry traces

Applications send traces using the **OpenTelemetry GenAI semantic conventions** (`gen_ai.*` attributes), which common instrumentation libraries already emit (OpenTelemetry SDKs, OpenLLMetry, framework integrations). No engine-specific SDK is needed.

- OTLP over HTTP (`POST /ingest/otlp/v1/traces`, protobuf and JSON) on the `ingest` listener, authenticated with an ingest token like HTTP push ([F08](F08-push-ingestion.md)). OTLP over gRPC is optional later.
- Each application is a device of type `llm_app`, resolved from the trace's `service.name` resource attribute. Unknown services are auto-registered within the tenant's device limit, as for MQTT sensors.
- Each `llm_app` gets a **`push.otlp` check**, as an MQTT sensor gets `push.mqtt`. It owns the trace metrics and has a last-seen rule (`expected_interval`, default 15 minutes, set per application), so an application that stops sending traces opens an incident like a silent sensor.
- Spans the engine understands: LLM calls (`gen_ai.operation.name` chat, completion, embeddings), retrieval, tool calls and agent steps. Other spans in the same trace are kept only as parents for context, without their attributes.
- A simple JSON event endpoint (`POST /ingest/v1/llm-events`) exists for applications that cannot use OpenTelemetry.

### Metrics from traces (phase 2)

Every LLM span becomes metric samples of its application, per model and per operation:

| Metric | Source |
| --- | --- |
| `requests`, `errors`, `rate_limited` | span status and error type |
| `latency_ms`, `ttft_ms` | span duration, first-token event |
| `input_tokens`, `output_tokens` | `gen_ai.usage.*` |
| `cost` | tokens × the tenant's model price table |
| `tool_calls`, `retrieval_ms`, `agent_steps` | child spans |

Thresholds, incidents and webhooks work on these like on any other metric: "error rate of `support-bot` above 5 % for 10 minutes", "cost per hour of `summarizer` above 20".

### Content capture (phase 3, opt-in)

Prompts and responses are needed for evaluation and debugging, and they often contain personal or confidential data.

- **Off by default.** Without capture, the engine stores span metadata only (model, tokens, timings, status, IDs).
- Turned on per application, with a **sample rate** (default 10 %) and **redaction rules** applied at ingestion before anything is stored: built-in detectors (email, phone, card numbers, Turkish national ID, IBAN) plus tenant regexes.
- Stored encrypted with the same envelope encryption as credentials ([ADR-0006](../adr/0006-credential-encryption.md)), with its own retention (default proposal 30 days), separate from metric retention.
- Readable only by tenant `admin` and a new `llm-reviewer` role; every read is written to the audit log. Content is never included in webhook events.

### Evaluators (phase 3)

Evaluators score captured spans. Scores become metrics of the application, so they get thresholds and incidents like everything else.

| Kind | Examples | Cost |
| --- | --- | --- |
| Deterministic | Valid JSON or schema match, empty or truncated answer, refusal detected, forbidden terms, PII in the response, answer length | Free, runs on every captured span |
| LLM-as-judge | Relevance, groundedness against the retrieved context (RAG), instruction following, toxicity, custom rubric | Tokens on a judge model chosen by the tenant, with its own credential; runs on a sample |
| User feedback | Thumbs up or down, rating, comment, sent by the application with the trace ID (`POST /v1/llm/feedback`) | Free |

- Evaluators are configured per application: which evaluator, sample rate, judge model, rubric.
- Judge calls go through the runner with the same timeouts, concurrency limits and SSRF rules as checks.

### Golden datasets: `llm.eval` (check, phase 3)

A scheduled regression test: a fixed set of prompts with expected answers or rubrics, run against an endpoint, scored by the evaluators.

- Catches quality changes from model upgrades, prompt changes or provider-side changes before users report them.
- Metrics: pass rate, mean score per evaluator, latency and cost of the run.
- Minimum interval 1 hour; the dataset size is capped per plan.

## Data

| Table | Contents |
| --- | --- |
| `llm_model_prices` | per tenant: provider, model, input and output price per million tokens, currency, valid from |
| `llm_spans` | partitioned by day: trace and span IDs, application device, model, operation, timings, tokens, cost, status, evaluator scores; no content |
| `llm_span_content` | encrypted prompt and response, redaction report; its own retention |
| `llm_evaluators`, `llm_datasets` | evaluator and golden dataset configuration per application |
| `llm_feedback` | feedback linked to trace and span IDs |

Span volume can be large. Storing spans in PostgreSQL is acceptable for phase 2 and 3 volumes; trace storage is the first candidate for the ClickHouse backend. A storage ADR is needed before phase 3 work starts.

## Billing

Proposed additions to [F13](F13-usage-metering.md):

| Unit | Class |
| --- | --- |
| `llm.inference`, `prometheus.scrape` | `advanced` check-hours |
| `llm.eval` | `advanced` check-hours plus judge tokens if the engine's judge is used |
| `push.otlp` (one per LLM application) | `push` check-hours ([F13](F13-usage-metering.md)) |
| Ingested LLM spans | new unit: per 1,000 spans, on top of the check-hours |
| Stored content | new unit: GB-days |
| LLM-as-judge evaluations | new unit: per evaluation (when the tenant uses a PlusClouds-provided judge model; free with the tenant's own model credential) |

## Security and privacy

- Content capture changes the engine's privacy profile: until now it stored almost no personal data. With capture on, it may store prompts containing personal data, so the [compliance documents](../compliance/README.md) must describe this, and KVKK/GDPR obligations (retention, deletion on request, processing agreements) apply to the operator.
- A request to delete a user's data must be able to remove captured content by a user or session attribute that the application sends.
- Judge models may be external services; sending captured content to them is a transfer of that data and must be a separate, explicit tenant setting.

## Acceptance criteria

- `llm.inference` against a local vLLM server reports TTFT and tokens per second; stopping the server opens an incident; a `429` produces WARNING, not CRITICAL.
- `prometheus.scrape` with the DCGM profile produces one object per GPU with utilization, memory and temperature.
- An application instrumented with the OpenTelemetry GenAI conventions appears as an `llm_app` device with request, latency, token and cost metrics within 10 s of its first trace.
- With capture off, no prompt or response text is stored anywhere (tested with marker strings, like credential secrets).
- With capture on, a prompt containing an email address and a card number is stored with both redacted.
- A golden dataset run against a deliberately degraded model opens a quality incident.

## Open questions

- Which LLM endpoints does PlusClouds run or sell today, and on which servers and GPUs? The first `prometheus.scrape` profiles should match them.
- Should PlusClouds offer a hosted judge model, or should tenants always bring their own?
- Trace volume per customer: needed to decide when trace storage moves to ClickHouse.
- Does the panel need a trace viewer, or are Grafana panels enough?
