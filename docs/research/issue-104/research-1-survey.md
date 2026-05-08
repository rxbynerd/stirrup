# Stirrup #104 — Researcher A: Industry Survey of LLM Trace/Eval Storage

**Scope.** Survey six named products plus the OpenTelemetry GenAI semantic conventions. For each, document the ingestion path, schema model, trace-vs-recording separation, eval/query surface, multi-tenancy, OTel relationship, self-host posture, and 2–3 specific things worth stealing or avoiding. The downstream architecture researcher will use this packet to ground recommendations on whether stirrup's `TraceLakehouse` should be retained, replaced by an OTel-derived projection, or split per backend.

**Date stamp.** Research conducted 2026-05-08. The LLM-observability space moves quickly; everything below cites a 2024–2026 source where one was reachable. Where a source could not be retrieved (broken doc URLs, vendor pages behind auth, etc.) I have flagged the claim explicitly.

**Reading guide.** Each vendor section follows the requested template verbatim. The synthesis at the end pulls cross-vendor patterns relevant to stirrup's specific question (kilobyte trace vs megabyte recording; CP-led control plane; batch eval-first access pattern, not APM live-query).

---

## 1. Langfuse

- **Architecture in one sentence**: A Postgres + ClickHouse + Redis + S3-compatible-blob v3 stack with a thin web tier and an async ingestion worker, where every event is durably written to S3 *first* and then promoted into ClickHouse for query [Langfuse self-hosting docs, 2025–2026; https://langfuse.com/self-hosting].
- **Ingestion path**: Two surfaces. (1) Native Python/JS SDKs (the v4 SDK is "a thin layer on top of the official OpenTelemetry client" and recommended for Python/JS) [https://langfuse.com/docs/opentelemetry/get-started]. (2) A direct OTLP endpoint (`/api/public/otel`) accepting **OTLP HTTP/JSON and HTTP/protobuf only** — gRPC is *not* supported [same source]. Tenant boundary on ingest is the API-key-scoped *project*; events are tagged with project ID at the front door before being written to S3 by the web tier and queued in Redis for the worker [Langfuse self-hosting].
- **Trace vs full conversation/recording separation**: There is **one logical store** (ClickHouse `observations` table) but full payloads (input/output messages, multi-modal attachments) are written to S3 first and only "a reference is persisted in Redis for queueing" [Langfuse self-hosting, 2025]. The async worker pulls payloads from S3 and ingests structured rows into ClickHouse. Multi-modal attachments stay on S3 indefinitely; they are never inlined into ClickHouse rows.
- **Schema model**: A deliberately *narrow, opinionated* schema centred on three nouns — `Trace`, `Observation` (with subtypes `generation`, `span`, `event`, `tool-call`, `retrieval`), and `Session` [https://langfuse.com/docs/tracing-data-model]. The current data model is "observation-centric" using "a single immutable Observations table" so context attributes propagate to every observation [same]. OTel attributes are mapped onto this; `langfuse.*` namespace attributes take precedence over generic GenAI conventions [https://langfuse.com/docs/opentelemetry/get-started]. Important caveat: this is *not* a thin OTel projection — it is a vendor schema that OTel feeds into.
- **Eval surface**: REST API + SDKs over the ClickHouse-backed read path (paginated `trace.list`, `observation.list`, etc.) [https://langfuse.com/docs/query-traces]. There is no public SQL endpoint. For bulk/eval workloads Langfuse explicitly recommends **Blob Storage Export** — a scheduled sync job that lands trace data in S3/GCS/Azure for downstream tools rather than paginating the API [same source]. PostHog and Mixpanel exports exist for analytics; no documented Iceberg or BigQuery-native export at the time of writing.
- **Multi-tenancy model**: Project-per-tenant inside a shared Postgres/ClickHouse cluster, with project-ID tagging on every row. Self-hosted single-org deployments are typical; Langfuse Cloud runs the same code with multi-org isolation [https://langfuse.com/self-hosting].
- **OTel relationship**: Accepts OTLP (HTTP only) but does **not** re-emit it. The official position is "use the Langfuse SDK if you can; OTel is the universal fallback" — they brand v4 as "OTEL-native" but it still ships extra mapping logic [https://langfuse.com/docs/opentelemetry/get-started].
- **Self-host vs SaaS**: Both supported. Self-host stack: Postgres + ClickHouse + Redis/Valkey + S3-compatible blob + 2 app containers (web + worker) [https://langfuse.com/self-hosting]. Operationally heavy compared to Phoenix.
- **What we should steal**:
  1. **S3-first ingestion**: every payload is durable on object storage *before* the OLAP store sees it; "even if the database is temporarily unavailable, the events are not lost" [self-hosting docs]. Maps cleanly onto stirrup's RunRecording/RunTrace asymmetry.
  2. **Reference + payload split**: queue carries a pointer to S3, not the payload. Avoids stuffing megabyte recordings through the OLAP pipeline twice.
  3. **Blob-storage export as the bulk-query escape hatch**: rather than building a heavyweight OLAP-export feature, expose a regular dump to S3/GCS that downstream eval tooling consumes directly.
- **What we should avoid**:
  1. Mapping OTel attributes onto a vendor-namespaced schema (`langfuse.*` overriding `gen_ai.*`) — locks consumers in and creates a parallel naming universe. Stirrup's `langfuse.*` analogue would be an `stirrup.*` namespace; resist that.
  2. The "single immutable Observations table" model assumes you control all instrumentation. If stirrup wants OTel as the wire, the table layout has to flex with semconv evolution — Langfuse's narrow schema makes that painful.
  3. Operating four storage systems for a single component — Postgres + ClickHouse + Redis + S3 is too much for a "lakehouse" if BigQuery or ClickHouse Cloud already supplies most of those layers.

---

## 2. LangSmith

- **Architecture in one sentence**: A multi-database SaaS / self-hosted stack that uses ClickHouse for traces and feedback, Postgres for transactional/operational state, Redis/Valkey for queueing, and optional cloud object storage (S3 / Azure Blob / GCS) for large trace artifacts and feedback attachments [https://docs.langchain.com/langsmith/architectural-overview].
- **Ingestion path**: Three. (1) The native LangSmith SDK (Python/JS), batched HTTP POSTs of the proprietary `Run` shape. (2) An OTLP endpoint at `https://api.smith.langchain.com/otel` accepting both **HTTP and gRPC** (HTTP/JSON and gRPC/protobuf), and explicitly documented as usable from non-LangChain apps [https://docs.langchain.com/langsmith/trace-with-opentelemetry]. (3) The auto-instrumented LangChain/LangGraph SDKs which call the same endpoint internally. The platform maps OTel GenAI attributes (`gen_ai.system`, `gen_ai.completion`, `gen_ai.usage.input_tokens`, role/content fields, tool-call attributes) into its internal Run model [same].
- **Trace vs full conversation/recording separation**: The model splits cleanly: ClickHouse holds run rows and feedback ("high-volume data"); Postgres holds organisation/project/dataset metadata; **blob storage is reserved specifically for "large files, such as trace artifacts, feedback attachments"** — i.e. exactly the recording-tier data [https://docs.langchain.com/langsmith/architectural-overview]. The blob store is optional in self-hosted deployments, which means small deployments inline blobs into ClickHouse and accept the cost.
- **Schema model**: Hierarchical — `Project → Trace → Run` where `Run` "is a span representing a single unit of work" [https://docs.langchain.com/langsmith/observability-concepts]. Hard cap of "25,000 runs" per trace [same]. Threads link traces by shared metadata keys (`session_id`, `thread_id`, `conversation_id`). Schema is fixed-but-extensible (`tags`, `metadata`, `feedback` are open dicts). OTel ingest is a *projection layer* on top of this, not the native shape.
- **Eval surface**: REST API and the LangSmith SDK. There is no documented public SQL surface; their `Datasets` and `Experiments` features are the eval-first surfaces, but they are queried via REST. *(I could not retrieve the precise limits page for run-size or attribute-size; flagged for verification.)*
- **Multi-tenancy model**: Workspaces → projects within an organisation. Self-host docs do not detail row-level filtering vs schema-per-tenant; the language ("almost everything besides traces and feedback" lives in Postgres) implies tenant-scoped row tagging in ClickHouse plus FK joins through Postgres [https://docs.langchain.com/langsmith/architectural-overview].
- **OTel relationship**: Best-in-class for *ingesting* OTel — full HTTP+gRPC, full GenAI semconv mapping. They do **not** re-emit OTel; downstream consumers use the LangSmith API, not OTLP [https://docs.langchain.com/langsmith/trace-with-opentelemetry].
- **Self-host vs SaaS**: Both. Self-host is Kubernetes-first for the full platform; "lightweight standalone servers" exist for smaller deployments [https://docs.langchain.com/langsmith/architectural-overview].
- **What we should steal**:
  1. **OTel as a co-equal first-class wire** alongside a native SDK — accept OTLP (HTTP *and* gRPC) and map known attributes to your internal schema rather than refusing OTel entirely. This is the lowest-friction way to onboard non-LangChain clients without giving up the richer native shape.
  2. **Cleanly split blob storage from the trace store** — `RunRecording` belongs in object storage just like LangSmith's "trace artifacts and feedback attachments". Don't make the OLAP store carry megabyte payloads.
  3. **Hard structural caps** like "25,000 runs per trace" — these prevent pathological agent loops from killing the index. Stirrup should consider analogous caps on `TurnTrace` count or recording size.
- **What we should avoid**:
  1. Two-database operational state (Postgres *and* ClickHouse *and* Redis *and* blob) is heavyweight if you only need eval queries. Stirrup probably doesn't need the Postgres + Redis tier separately — the lakehouse is OLAP-only.
  2. Tying the schema closely to a framework's mental model (`Run`, `LangGraph`-aware fields). If stirrup's ingest is OTel-only, it shouldn't grow `stirrup.run.*` fields with the same fervour LangSmith grew `langsmith.run.*` fields.
  3. Closed-box query surface. LangSmith offers REST-only access; for eval mining stirrup wants something closer to SQL/Iceberg-readable.

---

## 3. Braintrust

- **Architecture in one sentence**: A custom database called **Brainstore** that puts all data on object storage with a write-ahead-log front and an inverted-index + row-store + column-store back, designed specifically for AI traces where individual spans can exceed 1 MB and aggregate trace payloads reach tens of GB [https://www.braintrust.dev/blog/how-brainstore-works, April 2026; https://www.braintrust.dev/blog/brainstore, March 2025].
- **Ingestion path**: Native SDK (TypeScript/Python) with auto-instrumentation for OpenAI, Anthropic, Bedrock, Azure, Mistral, Together, Groq, LangChain, LangGraph, CrewAI, Pydantic AI, DSPy [https://www.braintrust.dev/docs/guides/traces]. OTel support is listed as one of "40+ integrations" but is not the primary ingestion path. Internally, writes "append to a write-ahead log for high throughput, then undergo asynchronous processing and compacting into indexed formats" [Brainstore architecture post]. Per-customer partitioning is enforced at the ingest layer.
- **Trace vs full conversation/recording separation**: Brainstore explicitly handles the size mismatch by treating object storage as the *primary* tier. Spans up to 1 MB+ are normal; "long-running traces (sometimes days) with out-of-order feedback and semi-structured data" are first-class. The inverted index + row store + column store sit *over* object-storage-resident data, not over a separate hot OLAP table [https://www.braintrust.dev/blog/how-brainstore-works].
- **Schema model**: Six span types — `eval`, `task`, `llm`, `function`, `tool`, `score` — each with `Input`, `Output`, metadata, metrics, scores [https://www.braintrust.dev/docs/guides/traces]. Critically, "logs use the same data structure as experiments", so production logs and offline evals share a schema; "instrumentation code works for both logging and evaluation" [https://www.braintrust.dev/docs/guides/logs]. Semi-structured fields are first-class (JSON-typed bag, not shredded into columns) [Brainstore post].
- **Eval surface**: BTQL, a pipe-syntax DSL that "is functionally equivalent to SQL"; the same parser accepts standard SQL too [https://www.braintrust.dev/docs/reference/btql]. Endpoint: `/btql`. Output: JSON or **Parquet**. Queryable: experiments, datasets, prompts/functions, project logs. Datasets can be derived from logs via SQL (the workflow exists but is not heavily documented). This is the only product in the survey that exposes a *first-class SQL-shaped query surface* against its analytics store.
- **Multi-tenancy model**: Per-customer data partitioning at the storage layer is a stated Brainstore principle ("each customer's data is partitioned separately to prevent performance degradation") [https://www.braintrust.dev/blog/how-brainstore-works]. In self-hosted deployments, the *entire data plane* (Postgres, Redis, object storage, Brainstore) lives in the customer's cloud; the control plane (UI, auth via Clerk, hashed API keys, organisation metadata) stays at Braintrust [https://www.braintrust.dev/docs/guides/self-hosting]. This is a true split control-plane / data-plane model.
- **OTel relationship**: They support OTel ingestion as one of many integrations but their pitch is unambiguous: traditional observability backends ("a traditional data warehouse") fail at AI workloads. Brainstore is positioned *against* a "use OTel and a generic warehouse" approach [Brainstore post]. They do not appear to re-emit OTel.
- **Self-host vs SaaS**: Both. Self-host is heavyweight: Brainstore demands "high-performance storage with at least 150,000 IOPS for both reads and writes" using NVMe ephemeral storage; Postgres needs 8+ vCPUs / 64 GB / 1000 GB / 15,000 IOPS for production [https://www.braintrust.dev/docs/guides/self-hosting]. Terraform modules for AWS/GCP/Azure exist.
- **What we should steal**:
  1. **One schema shared by production logs and eval experiments** — "logs use the same data structure as experiments" means the same code instruments both [https://www.braintrust.dev/docs/guides/logs]. Stirrup's `RunTrace` and the eval `RunResult` should share enough structure that mining a failure into an eval task is mechanical (which is already issue #104's spirit).
  2. **A SQL-equivalent query endpoint that returns Parquet directly** — for an eval CLI, this is the most ergonomic possible interface. Beats paginated REST every time.
  3. **Object storage as the primary tier, with multiple indexes layered on top** — directly applicable to RunRecording (megabyte) vs RunTrace (kilobyte): both live on S3, the index decides what's cheap.
- **What we should avoid**:
  1. **Building a custom database**. Brainstore took 18+ months and a team. For a coding-agent harness with one or two reference deployments, the cost is unjustifiable. Use ClickHouse or BigQuery; do not build Brainstore.
  2. **150k IOPS NVMe self-host requirement** — this is a hard sell for any operator who isn't running on bare metal or i3en instances. Stirrup's lakehouse should be cheap to self-host, not a database project.
  3. **Tight coupling between SDK and instrumentation magic**. Braintrust's auto-instrumentation across 40+ frameworks is a maintenance commitment; OTel-native instrumentation is the cheaper long-term path for a small team.

---

## 4. Helicone

- **Architecture in one sentence**: A primarily-proxy LLM gateway whose backend stack is **Cloudflare Workers (proxy) + Jawn (Express log collector) + Supabase/Postgres (auth/app DB) + ClickHouse (analytics) + MinIO/S3 (raw request/response bodies)** [https://github.com/Helicone/helicone, README].
- **Ingestion path**: Three. (1) **Proxy mode** (the default): change your base URL to `https://ai-gateway.helicone.ai` and Helicone sees every request/response on the wire [https://docs.helicone.ai/getting-started/quick-start]. (2) **Async/OpenLLMetry mode**: a language-specific logger ships events out-of-band so Helicone is not in the critical path; OTLP-direct ingestion is *not* documented [https://docs.helicone.ai/getting-started/integration-method/openllmetry]. (3) A **manual logger** API for cURL/TS/Py/Go that POSTs request+response payloads directly. Tenant boundary is Helicone-API-key-scoped; auth via Supabase.
- **Trace vs full conversation/recording separation**: Yes — explicit. Structured analytics rows go to ClickHouse; full request/response bodies go to MinIO/S3 ("object storage for logs"); user/auth/app metadata goes to Supabase Postgres [Helicone GitHub README]. This is the closest architectural twin to what Langfuse v3 settled on.
- **Schema model**: Centred on a single `Request` row with structured columns (latency, cost, model, tokens, status) plus references to body blobs in object storage. Sessions and properties are tag/metadata layers on top [https://docs.helicone.ai/getting-started/integration-method/openllmetry]. Token-count fields accept multiple provider conventions (`prompt_tokens` / `input_tokens` / `promptTokenCount`) [https://docs.helicone.ai/getting-started/integration-method/custom]. **No public OTel GenAI semconv adherence** documented.
- **Eval surface**: Helicone Query Language ("HQL") on the Pro tier upward [https://www.helicone.ai/pricing]. Eval is not the platform's primary use case; their pitch is gateway + observability + caching + experiments-as-A/B.
- **Multi-tenancy model**: Organisation → project hierarchy via Supabase auth. Team tier supports 5 orgs; Enterprise supports SAML SSO and on-prem [pricing page].
- **OTel relationship**: Weakest of the six. The closest thing is the OpenLLMetry-based async logger, which is **not** OTLP-direct. None of the docs I could reach claim OTLP ingest support [https://docs.helicone.ai/getting-started/integration-method/openllmetry]. Helicone is a proxy-first product; OTel was an afterthought.
- **Self-host vs SaaS**: Both. Self-host via Docker Compose (dev) or Helm (enterprise). Components: Web (NextJS), Worker (Cloudflare Workers), Jawn (Express), Supabase, ClickHouse, MinIO [GitHub README].
- **What we should steal**:
  1. **The proxy-mode-versus-async-mode distinction**. For a coding-agent harness this maps onto "the harness already has the bytes, so async/manual ingest is the right model" — there is no value in proxying.
  2. **Object-storage-for-bodies + ClickHouse-for-rows + Postgres-for-app-state** — this exact triple is Helicone, Langfuse v3, and (by config) LangSmith. Strongest signal in the survey.
  3. **Multi-provider token-count normalisation** (accepting OpenAI's `prompt_tokens`, Anthropic's `input_tokens`, Google's `promptTokenCount` interchangeably). Stirrup already normalises but worth confirming our shape covers all three.
- **What we should avoid**:
  1. **Five microservices for a single observability product** (Web, Worker, Jawn, Supabase, ClickHouse, MinIO is six). Stirrup's lakehouse component should be one service against managed dependencies.
  2. **Proxy-mode as the default**. For a server-side coding agent, putting an HTTP proxy on the model-call path adds latency, a single point of failure, and a key-handling surface that doesn't need to exist.
  3. **Cloudflare Workers for ingestion**. Vendor-specific runtime shape limits the deployment story; better to ingest in the same process as the rest of the platform.

---

## 5. Arize Phoenix (OSS) and Arize AX (commercial)

- **Architecture in one sentence**: Phoenix is "built on top of OpenTelemetry and powered by OpenInference instrumentation," with SQLite or PostgreSQL as the *only* persistent store and OpenInference span attributes as the schema; AX is the commercial superset that handles "terabytes of data and billions of spans" via a managed collector and unspecified backend [https://github.com/Arize-ai/phoenix; https://arize.com/docs/ax/observe/tracing].
- **Ingestion path**: **OTLP-native, both products**. Phoenix accepts OTLP over gRPC and HTTP via `arize-phoenix-otel` (a thin OTel SDK wrapper) and any OpenInference-instrumented framework [https://github.com/Arize-ai/phoenix]. AX accepts OpenInference spans "via the OpenTelemetry Protocol (OTLP) over gRPC" through a managed collector [https://arize.com/docs/ax/observe/tracing]. Tenant boundary on ingest: Phoenix is single-tenant per instance; AX uses managed multi-tenant projects.
- **Trace vs full conversation/recording separation**: **None on Phoenix** — full prompt/completion content lives as OpenInference span attributes in SQLite/Postgres, the same place as the structured fields. This is the cheapest possible model and works because OpenInference attributes are size-bounded by the span. AX's separation is undocumented in the pages I could reach.
- **Schema model**: **Pure OpenInference + OTel** — no vendor schema layer. OpenInference is "owned" by Arize and is positioned as "the industry standard" alongside the OTel GenAI semconv [https://arize.com/docs/ax/observe/tracing]. OpenInference covers `input.value`, `output.value`, `llm.model_name`, `llm.token_count.prompt`, retrieval/embedding fields, etc. — pre-dating much of OTel GenAI and partially overlapping it.
- **Eval surface**: Phoenix has its own evals package (`arize-phoenix-evals`) plus a notebook-driven flow; queries go through the Python client against Postgres/SQLite. AX has an "Evaluate" product and an "Alyx" AI-assisted debugger [https://arize.com/docs/ax]. Both treat eval as a first-class workflow against the same trace store; mining failures into datasets is supported.
- **Multi-tenancy model**: **Phoenix is explicitly single-tenant per instance** — "deploy multiple Phoenix instances" for multi-team scenarios [https://arize.com/docs/phoenix/self-hosting/architecture]. They scale horizontally by either (a) multiple Phoenix instances behind a load balancer sharing one Postgres, (b) separate instances + databases for env isolation, or (c) **multiple instances using different schemas in one Postgres** (`PHOENIX_SQL_DATABASE_SCHEMA`) [same]. AX provides true multi-tenancy with SSO and RBAC; resource-tag-based access control is on the 2026 roadmap for Phoenix [same].
- **OTel relationship**: **The most OTel-native of the survey.** Phoenix's wire format *is* OTLP carrying OpenInference attributes; the schema and the wire are the same thing. AX adds a managed collector but keeps the same shape.
- **Self-host vs SaaS**: Phoenix self-host is trivial — single container, SQLite default, Postgres optional, no Redis, no S3 required [https://arize.com/docs/phoenix/self-hosting]. AX is SaaS-only (with on-prem available for enterprise per general industry pattern; not confirmed in the docs I could reach).
- **What we should steal**:
  1. **Wire format == schema**. OpenInference + OTel as both the ingest format and the storage shape removes an entire mapping layer. This is the most stirrup-aligned architectural choice in the survey.
  2. **Schema-per-tenant in one Postgres** as a pragmatic multi-tenancy escape hatch (`PHOENIX_SQL_DATABASE_SCHEMA`). Cheaper than per-tenant clusters, harder to mis-configure than RLS.
  3. **Single-container default with SQLite, optional Postgres** — the "I just want to try it" path is one `docker run`. Stirrup's lakehouse should have a similar zero-config local mode.
- **What we should avoid**:
  1. **Storing megabyte payloads as OTel span attributes against SQLite/Postgres** — fine for kilobyte LangChain spans, terrible for stirrup's RunRecording shape. Need a blob escape hatch that Phoenix lacks.
  2. **Single-tenant-by-instance** as the only multi-tenancy model. For a CP-led world stirrup wants real tenant isolation, not "deploy more containers."
  3. **Owning a competing semantic convention** (OpenInference vs OTel GenAI). The two largely overlap; keeping both alive is a tax. Stirrup should pick one and (per current OTel direction) it should be `gen_ai.*`.

---

## 6. Honeycomb (GenAI features)

- **Architecture in one sentence**: A general-purpose distributed-tracing backend (the proprietary Retriever column store) that has *added* GenAI affordances — semconv-aware UI decorations, a "Gen AI attributes view," and integrations with eval tools like Braintrust — without altering the underlying ingest or storage model [https://honeycomb.io/blog/honeycomb-is-built-for-the-agent-era-pt1, April 2026; https://honeycomb.io/blog/fast-ai-feedback-loops-honeycomb-opentelemetry, April 2026].
- **Ingestion path**: **OTLP only** (gRPC, HTTP/protobuf, HTTP/JSON) to `api.honeycomb.io:443` (US) or `api.eu1.honeycomb.io:443` (EU), authenticated by `x-honeycomb-team` header [https://docs.honeycomb.io/send-data/opentelemetry/]. There is no native SDK that bypasses OTel. They consume OTel GenAI semconv versions 1.37.0–1.40.0 [https://honeycomb.io/blog/fast-ai-feedback-loops-honeycomb-opentelemetry].
- **Trace vs full conversation/recording separation**: **No deliberate separation**. The Honeycomb model is "wide events" — a single span carries every relevant attribute, including (for LLM use cases) full input/output strings as span attributes. Their LLM-observability blog posts state that they capture "user input text, full LLM response strings, token counts, configuration parameters" all on the span itself [https://www.honeycomb.io/blog/observability-llm-applications]. Field-size limits exist but I could not retrieve the specific numbers — the docs URL I needed (`reference/api/limits`) returned 404. *[Flagged: verify size limits independently — Honeycomb's documented per-attribute and per-event size caps are the architectural breaking point for storing megabyte recordings.]*
- **Schema model**: **OTel spans only**, with no opinionated GenAI schema beyond decorating known `gen_ai.*` attributes. The platform is "vendor-agnostic" by design and treats GenAI as one shape among many [https://honeycomb.io/use-cases/ai-llm-observability]. This is the *most* schema-permissive option in the survey: anything you can put on an OTel span is queryable.
- **Eval surface**: Not eval-first. Honeycomb is an APM-style live-query tool (BubbleUp, query assistant, SLOs). They integrate *with* Braintrust for eval [https://honeycomb.io/blog/honeycomb-is-built-for-the-agent-era-pt1] rather than competing with it. Bulk export is via S3-export contracts on enterprise plans (general industry knowledge — not directly confirmed in the pages I retrieved). *[Flagged for verification.]*
- **Multi-tenancy model**: Team / environment hierarchy at the org level; ingest tenant boundary is the API key. Internal storage uses tenant tagging in Retriever; not a per-tenant dataset model.
- **OTel relationship**: **OTel is the only wire**, and they actively contribute to the GenAI semconv. They neither re-emit nor wrap OTel — apps emit standard OTLP and Honeycomb consumes it directly [https://docs.honeycomb.io/send-data/opentelemetry/].
- **Self-host vs SaaS**: SaaS only. No self-host option.
- **What we should steal**:
  1. **OTel-only-on-the-wire**. If stirrup's harness already emits OTel, Honeycomb is proof you can build a real product on top of "OTel spans, full stop." Directly relevant to the question of whether `TraceLakehouse` is needed.
  2. **Wide events / full-attribute spans for the *trace* tier**. For sub-megabyte traces this is the simplest possible model and avoids any blob-tier complexity.
  3. **Decorations over schema**. Honeycomb adds GenAI affordances at the *UI* layer (icons, tooltips, attribute views) rather than reshaping the storage. Keeps the storage layer generic.
- **What we should avoid**:
  1. **Attribute-only storage for megabyte recordings**. Honeycomb's wide-events model breaks down at stirrup's recording size. We need a blob escape hatch they don't provide.
  2. **No first-class eval surface**. For stirrup specifically, the absence of `mine-failures`-style workflows is a gap that other vendors (LangSmith, Braintrust, Phoenix) fill natively.
  3. **SaaS-only deployment**. For a CP-led control plane that may want to stay inside a customer's cloud, no-self-host is a hard blocker.

---

## 7. OpenTelemetry GenAI semantic conventions (mid-2026)

- **Status**: All GenAI conventions are formally in **Development** stability as of mid-2026. The only attributes within the GenAI namespace currently marked **Stable** are the cross-cutting OTel attributes (`error.type`, `server.address`, `server.port`) [https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/; https://opentelemetry.io/docs/specs/semconv/gen-ai/openai/]. Versions 1.37.0–1.40.0 are the active ones being adopted by vendors like Honeycomb [https://honeycomb.io/blog/fast-ai-feedback-loops-honeycomb-opentelemetry]. The opt-in flag for tracking the latest experimental conventions is `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental` [https://opentelemetry.io/docs/specs/semconv/gen-ai/].
- **Specified span types** [https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/; https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/]:
  - **Inference** (model client calls)
  - **Embeddings**
  - **Retrievals** (vector DB / search)
  - **Execute Tool**
  - **Create Agent** (CLIENT kind, remote agent service init)
  - **Invoke Agent** (CLIENT for remote, INTERNAL for in-process)
  - **Invoke Workflow** (INTERNAL, multi-agent coordination)
- **Required attributes**: `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `server.address`/`port` for sampling decisions; `error.type` on failure.
- **Recommended attributes** (selected): `gen_ai.request.{temperature, top_k, top_p, frequency_penalty, presence_penalty, max_tokens, choice_count, seed, stream, stop_sequences}`; `gen_ai.usage.{input_tokens, output_tokens, cache_read.input_tokens, cache_creation.input_tokens, reasoning.output_tokens}`; `gen_ai.tool.{name, type, description, definitions, call.id, call.arguments, call.result}`; `gen_ai.agent.{id, name, version, description}`; `gen_ai.workflow.name`; `gen_ai.conversation.id`; `gen_ai.evaluation.{name, score.value, score.label, explanation}` [https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/].
- **Content representation — the key design question**: The spec offers three coexisting modes, with explicit guidance to prefer events for content: [https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/; https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-events/]
  1. **Span attributes** (`gen_ai.system_instructions`, `gen_ai.input.messages`, `gen_ai.output.messages`) — opt-in, recorded "structured if supported, else JSON string." Privacy/size concerns explicitly called out.
  2. **Events** (`gen_ai.client.inference.operation.details`, `gen_ai.evaluation.result`) — also opt-in; events MUST be recorded in structured form. Both events are still **in development** and not universally available across language SDKs.
  3. **External storage hooks** — instrumentations "may support hooks for uploading content separately, enabling independent access controls" [span spec]. This is the loophole that legitimises stirrup's blob/object-storage tier — the spec explicitly anticipates large content being moved off-span.
- **Provider-specific extensions**: Stable extension namespaces exist for `openai.*`, `anthropic.*`, `azure.ai.inference.*`, `aws.bedrock.*`, plus an MCP-specific convention bundle [https://opentelemetry.io/docs/specs/semconv/gen-ai/]. Provider-specific attributes inherit the same Development status as the parent GenAI conventions.
- **What's left open / undefined**:
  - **Token usage in streaming + multi-turn agent loops**: how to attribute cache-hit tokens across nested spans is unspecified.
  - **Tool-call result schemas**: `gen_ai.tool.call.result` is a free-form string/JSON; no enforced shape.
  - **Cross-span aggregation for full conversations**: no semconv field for "all spans belonging to one logical conversation/run" beyond `gen_ai.conversation.id`. Stirrup's `ParentRunID` for sub-agent fan-in (issue #55) is a *natural extension* but not standardised.
  - **Agent-framework spans** beyond `invoke_agent` / `invoke_workflow` are still being defined per the 2025 OTel blog post [https://opentelemetry.io/blog/2025/ai-agent-observability/].
  - **Content size/truncation policy**: spec defers entirely to instrumentation/backend.
- **Practical implication for stirrup**: The OTel GenAI semconv covers the *RunTrace*-tier shape adequately if stirrup is willing to accept Development-stability fields. It does **not** prescribe how to store *RunRecording*-tier content; the "external storage hooks" sentence is the only normative guidance, and it points exactly toward blob storage. So OTel-as-the-only-wire is feasible for traces, but the recording tier is a vendor-specific decision OTel doesn't (and shouldn't) make.

---

## Synthesis

### The trace-vs-recording size-mismatch architecture

The dominant pattern across the four products that have publicly described their internals — Langfuse v3, LangSmith, Helicone, Braintrust — is identical in shape: **structured rows in a columnar OLAP store, full payloads in object storage, transactional metadata in Postgres**. The triple is so consistent that it is effectively a reference architecture:

| Product | OLAP store | Blob store | Transactional |
|---|---|---|---|
| Langfuse v3 | ClickHouse | S3-compatible (any) | Postgres + Redis |
| LangSmith | ClickHouse | S3 / Azure Blob / GCS (optional) | Postgres + Redis |
| Helicone | ClickHouse | MinIO/S3 | Supabase (Postgres) |
| Braintrust | Brainstore (custom, on object storage) | object storage (same tier) | Postgres + Redis |

Phoenix is the outlier: it stores everything as OTel/OpenInference attributes in Postgres or SQLite, which works because typical LangChain-ish spans are kilobytes, not megabytes. Honeycomb is the other outlier: full content goes on the span as wide-event attributes, against undocumented (to me) field-size limits. **For stirrup's RunRecording-megabyte tier, the Phoenix and Honeycomb models do not survive scaling**; the four-vendor consensus on object-storage-for-blobs is the right place to start.

The deeper signal: Langfuse explicitly writes the payload to S3 *before* anything else, then queues a reference. Helicone does the same conceptually (Jawn → MinIO + ClickHouse). Braintrust's Brainstore is "designed for AI-shaped logs in object storage" as a first-class principle, not a tier-down. The lesson is that even in vendors that look like "ClickHouse with extras," **object storage is increasingly the system of record and the OLAP store is treated as an index into it**.

### "OTel as the only wire" vs vendor SDK with optional OTel export

The industry has *not* converged on one answer. The options form a clean spectrum:

- **OTel-only**: Honeycomb, Phoenix/AX. Wire format is OTLP; schema is `gen_ai.*` (Honeycomb) or OpenInference + `gen_ai.*` (Phoenix/AX). Lowest mapping cost.
- **OTel co-equal with native SDK**: LangSmith, Langfuse v4. Both accept OTLP as a first-class endpoint and map known attributes onto a vendor schema. Native SDK preferred for richer fields.
- **Native SDK primary, OTel via integration**: Braintrust. OTel is "one of 40+ integrations." The vendor schema (six span types, semi-structured payloads >1 MB) is the priority.
- **Proxy primary, OTel almost absent**: Helicone. Closest thing to OTel is OpenLLMetry async, which is *not* OTLP-direct.

Crucially: **none of the six products re-emit OTel.** Once data is in their store, it leaves via REST, SDK, BTQL, blob export, or product-specific integrations. The "OTel is the wire" pattern goes one direction only.

For stirrup, the practical takeaway is that "accept OTLP" and "be schema-compatible with `gen_ai.*` semconv" are the cheapest invitations to the existing ecosystem, but neither commits stirrup to *only* OTel internally. The RunTrace shape can be a `gen_ai.*` projection; the RunRecording shape lives outside semconv anyway.

### Eval-style batch access vs APM live-query

This is the cleanest cleavage in the survey. Three vendors treat the analytics store as a **separate concern from the live span store**:

- **Braintrust** treats it as the *only* concern: Brainstore *is* the analytics store, with object storage as primary tier and indexes layered on. BTQL/SQL with Parquet output is the canonical eval surface.
- **Langfuse** keeps live ClickHouse for product features and recommends **Blob Storage Export** as the bulk/eval path — explicitly acknowledging that paginated REST is wrong for batch workloads.
- **LangSmith** has Datasets + Experiments as a parallel feature stack on top of the same trace store, with REST as the access surface.

Two vendors do *not* split: Phoenix uses one store for both, accepting the limit; Honeycomb is APM-shaped with no native eval surface and integrates externally (Braintrust).

For stirrup specifically — where eval is *batch / OLAP, not APM live-query* — the Braintrust and Langfuse-blob-export patterns are the most relevant. A `TraceLakehouse` that is queryable via SQL (BigQuery) or Iceberg-readable (ClickHouse Cloud → Iceberg) maps cleanly onto these. **The OTel-derived projection alone, with no separate analytics store, looks insufficient for `mine-failures`-style workloads** unless OTel data is replicated into a query-friendly analytics warehouse anyway — at which point you have a lakehouse, just by another name.

### Multi-tenancy patterns

Three patterns appear:

1. **Project-per-tenant in a shared cluster, row-tagged**: Langfuse, LangSmith, Helicone, Honeycomb. Cheapest to operate; tenant boundary is the API key + a row-level WHERE.
2. **Per-customer storage partitioning**: Braintrust's Brainstore explicitly partitions per customer at the object-storage layer. Stronger isolation; harder to scale operationally for many small tenants.
3. **One-instance-per-tenant (or schema-per-tenant in one Postgres)**: Phoenix self-host. Hardest isolation, lowest density.

There is **no widespread RLS-in-Postgres pattern** in this segment of the market, despite RLS being well-supported in the underlying databases. The practical reason is that ClickHouse — the OLAP store of choice — does not have a comparable RLS model, so vendors solve it at the application/query layer.

For stirrup with BigQuery or ClickHouse Cloud as the target, the project-per-tenant + row-tagged model is the path of least resistance. BigQuery datasets-per-tenant is a viable variant if true storage isolation is needed.

### Open table format export (Iceberg / Delta)

Almost entirely **absent** from the survey. The closest match is:

- **Braintrust BTQL** can return query results in **Parquet** format [https://www.braintrust.dev/docs/reference/btql] — the file format under Iceberg but not the catalog. No Iceberg metadata, no schema evolution, no time-travel.
- **Langfuse Blob Storage Export** dumps to S3/GCS/Azure on a schedule [https://langfuse.com/docs/query-traces], format unspecified in the page I could reach (general industry pattern is JSON or Parquet line files). Not Iceberg-native.
- **LangSmith** has optional blob storage but no documented Iceberg/Delta export.
- **Phoenix, Honeycomb, Helicone**: nothing public.

The implication is that **stirrup committing to BigQuery or ClickHouse Cloud puts it ahead of every product in this survey on the open-table-format dimension** — both targets have first-class Iceberg/external-table support, and neither is matched by any of the six surveyed vendors today. This is the one area where stirrup's question ("BigQuery or ClickHouse Cloud as the production backing store") is actually *ahead* of the LLM-observability industry rather than catching up to it. *(General-domain knowledge: BigQuery's BigLake / managed Iceberg and ClickHouse Cloud's Iceberg-table-engine support are both GA as of 2025; verify exact feature levels independently.)*

---

## Sources & references

| Source | Title | Date / version | URL |
|---|---|---|---|
| Langfuse | Self-hosting architecture | 2025–2026 | https://langfuse.com/self-hosting |
| Langfuse | OpenTelemetry get-started | 2025–2026 | https://langfuse.com/docs/opentelemetry/get-started |
| Langfuse | Tracing data model | 2025–2026 | https://langfuse.com/docs/tracing-data-model |
| Langfuse | Query traces | 2025–2026 | https://langfuse.com/docs/query-traces |
| LangSmith | Architectural overview | 2025–2026 | https://docs.langchain.com/langsmith/architectural-overview |
| LangSmith | Trace with OpenTelemetry | 2025–2026 | https://docs.langchain.com/langsmith/trace-with-opentelemetry |
| LangSmith | Observability concepts | 2025–2026 | https://docs.langchain.com/langsmith/observability-concepts |
| Braintrust | How Brainstore works | 2026-04-06 | https://www.braintrust.dev/blog/how-brainstore-works |
| Braintrust | Brainstore launch | 2025-03-03 | https://www.braintrust.dev/blog/brainstore |
| Braintrust | Tracing guide | 2025–2026 | https://www.braintrust.dev/docs/guides/traces |
| Braintrust | Logs guide | 2025–2026 | https://www.braintrust.dev/docs/guides/logs |
| Braintrust | BTQL reference | 2025–2026 | https://www.braintrust.dev/docs/reference/btql |
| Braintrust | Self-hosting guide | 2025–2026 | https://www.braintrust.dev/docs/guides/self-hosting |
| Helicone | GitHub README | 2025–2026 | https://github.com/Helicone/helicone |
| Helicone | Quick start | 2025–2026 | https://docs.helicone.ai/getting-started/quick-start |
| Helicone | OpenLLMetry integration | 2025–2026 | https://docs.helicone.ai/getting-started/integration-method/openllmetry |
| Helicone | Manual logger | 2025–2026 | https://docs.helicone.ai/getting-started/integration-method/custom |
| Helicone | Pricing | 2025–2026 | https://www.helicone.ai/pricing |
| Phoenix | GitHub README | 2025–2026 | https://github.com/Arize-ai/phoenix |
| Phoenix | Self-hosting architecture | 2025–2026 | https://arize.com/docs/phoenix/self-hosting/architecture |
| Arize AX | Tracing docs | 2025–2026 | https://arize.com/docs/ax/observe/tracing |
| Arize AX | Product overview | 2025–2026 | https://arize.com/docs/ax |
| Honeycomb | Built for the agent era | 2026-04-06 | https://honeycomb.io/blog/honeycomb-is-built-for-the-agent-era-pt1 |
| Honeycomb | Fast AI feedback loops with OTel | 2026-04-20 | https://honeycomb.io/blog/fast-ai-feedback-loops-honeycomb-opentelemetry |
| Honeycomb | Observability for LLM apps (older) | ~2024 | https://www.honeycomb.io/blog/observability-llm-applications |
| Honeycomb | Send data via OpenTelemetry | 2025–2026 | https://docs.honeycomb.io/send-data/opentelemetry/ |
| Honeycomb | LLM observability use case | 2025–2026 | https://honeycomb.io/use-cases/ai-llm-observability |
| OpenTelemetry | GenAI semconv index | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/gen-ai/ |
| OpenTelemetry | GenAI span conventions | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/ |
| OpenTelemetry | GenAI agent span conventions | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/ |
| OpenTelemetry | GenAI events conventions | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-events/ |
| OpenTelemetry | GenAI attributes registry | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/ |
| OpenTelemetry | OpenAI provider conventions | 2025–2026 | https://opentelemetry.io/docs/specs/semconv/gen-ai/openai/ |
| OpenTelemetry | AI agent observability blog | 2025 | https://opentelemetry.io/blog/2025/ai-agent-observability/ |

### Sources I could not verify or retrieve

The following pages returned 404/500 during research and the corresponding claims should be independently verified before use:

- Honeycomb attribute/event size limits — `docs.honeycomb.io/reference/api/limits/` and similar paths.
- Langfuse v3 stack-evolution blog post (`langfuse.com/blog/2024-12-langfuse-v3-stack`) — architecture details inferred from current self-hosting docs instead, which are consistent.
- LangSmith run/blob size limits page (`docs.langchain.com/langsmith/limits`).
- Helicone ClickHouse-migration blog (`helicone.ai/blog/clickhouse-migration`) — ClickHouse role inferred from the GitHub README's service list, which is consistent.
- Braintrust resilient-observability and self-hosting blog posts referenced by title in their blog index.

These gaps do not change the core findings — the Object Storage + ClickHouse + Postgres triple, Brainstore's object-storage-primary model, and the OTel-only-wire vs vendor-SDK spectrum are confirmed from multiple independent sources within each vendor's docs.
