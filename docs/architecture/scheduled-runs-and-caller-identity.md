# ScheduledRun and caller identity: design decision

Decision implemented: 2026-10-25. This clean-install API replaces the earlier
creator-owned schedule configuration. See [credential injection](credential-injection.md)
for the runtime transport and operational limits.

## Decision

Move ScheduledRun configuration to a Kubernetes CRD while retaining durable
execution state in PostgreSQL and task/history semantics in upstream A2A.

Use one credential header configuration model on ModelConfig, RemoteMCPServer, and
EgressPolicy. Token passthrough propagates the authenticated invocation's
credential, whether it represents a human or a machine. Scheduled invocations
use kagent's service-account credential as their caller credential.

Describe callers through one stable, opaque principal ID and authenticated
attributes, and keep three concepts independent:

| Concept | Meaning | Scheduled invocation |
| --- | --- | --- |
| Invocation principal and credential | Who initiated the invocation and whose token a passthrough binding uses | The kagent service account |
| Runtime identity | Which verified Substrate Actor is executing, bound to its Session and Agent | The execution's Actor |
| Resource access context | Persisted creator identity and Agent/schedule/namespace references evaluated by authorization policy | SA creator identity and the scheduled execution's resource references |

The creator of a CRD does not supply a durable execution identity. Persisted
creator information does not select a credential for later calls. A machine
identity can carry a token and use the same passthrough mechanism as a human.

## Principal IDs, authenticated attributes, and resource access

Use one authenticated principal representation for callers. Its public identity
is a stable, opaque `Principal.ID`. Keep `Session.creator` as a single principal
ID, removing the assumption that it identifies a human. There is no user/Agent
owner union, separate issuer/subject field pair, or SPIFFE requirement.

The AuthProvider verifies credentials and returns the normalized principal ID
and authenticated attributes used by authorization. It owns three invariants:

- The same principal keeps the same ID across credential rotation.
- Different principals do not collide, including across authentication sources.
- Callers cannot assert an ID through an unverified request field.

Source-specific identity details remain private to authentication. Examples of
how the interface can accommodate different mechanisms are:

| Mechanism | Authentication provider's identity mapping |
| --- | --- |
| OIDC/JWT | Verified issuer and subject |
| Kubernetes SA token | Verified service-account identity within its cluster |
| API key | Account associated with the key, independent of the key's value |
| Client certificate | Principal established by the configured certificate mapping |

Only the mechanisms needed for the current task need implementation. No public
principal-ID syntax or new principal registry is required by this decision.
Consumers compare opaque IDs; they do not parse an authentication-provider name,
JWT claim, or resource reference out of them. Preserve existing human IDs where
the provider's uniqueness contract permits it. Define provider-specific identity
replacement semantics where the same source name can be reused.

When creating a Session, the service records the authenticated principal's ID
in `creator` and preserves the existing Agent binding and scheduled-execution
provenance. Do not copy bearer tokens or mutable authentication attributes into
durable ownership fields. Current caller credentials are acquired independently
of the stored creator ID.

Authorization combines the current caller's authenticated attributes, requested
operation, and trusted stored resource context. Example policies are:

- A caller matching a Session's recorded creator can access it subject to the
  configured authorization policy. The rule works for human and SA subjects.
- A caller with an authorized support-operator attribute can read scheduled Sessions
  in the support namespace. This does not automatically expose other users'
  private Sessions or permit execution.
- A verified Actor can use its bound Session through the existing restricted
  runtime policy, even though its own subject differs from the Session creator.

The Session namespace and schedule association come from stored resource
references. Policy assigns meaning to attributes supplied by a trusted
authenticator. Access checks use attributes from the current authenticated
request, rather than permissions copied from the creation token. Freshness and
revocation follow the authentication mechanism's contract. The recorded creator
ID remains useful for attribution even when access is revoked.

All scheduled calls using the same SA have the same caller identity. Caller
attributes cannot distinguish those schedules on their own; namespace, Agent UID, and
schedule UID remain necessary resource context. This is why removing the owner
union does not mean removing data scoping or granting global access.

## Implemented identity mapping and credentials

Built-in trusted-proxy authentication namespaces the configured identity claim
(or its existing `sub` fallback) by the authenticated issuer. The trusted proxy
must verify the token and restrict access to the controller; parsing claims does
not verify a signature. Request `X-User-Id`, `user_id`, and `X-Agent-Name` cannot
override that identity. This changes trusted-proxy creator IDs on clean installs;
there is no automatic adoption of historical sessions under raw subject IDs.
Insecure development mode retains caller-selected IDs and provides no identity
assurance.

`X-User-Id` forwarded to integrations also carries this opaque principal ID in
authenticated modes. Consumers must not interpret it as a raw user ID. Insecure
mode retains the caller-selected value.

Kubernetes authentication uses TokenReview with `server.auth.serviceAccount.audience`.
It maps the verified SA UID under the configured stable cluster `authority` into
an opaque ID. Token rotation preserves the ID; deleting and recreating an SA
changes it. Configure distinct authorities for distinct Kubernetes authentication
sources sharing a store, and keep the authority stable across controller restarts.

Helm projects separate inbound (`kagent-invocation`) and outbound (`kagent-egress`)
tokens, in addition to the Kubernetes API credential, and grants TokenReview
access. The scheduler authenticates its principal using the inbound token and
exports only the outbound token. Both must identify the same SA UID. Equal
audiences and outbound tokens also carrying the inbound audience are rejected;
receiving services cannot replay the outbound token to authenticate to Kagent.
Kubelet rotates the projections. The scheduler reads and verifies the outgoing
token for each new invocation, rejects a changed SA UID, and returns errors
without token material. External SA callers
use the same verifier; their supplied credential is reverified before forwarding.
Custom authentication providers retain their own forwarding contract.

The requested downstream TTL is configurable in Helm:

```yaml
config:
  scheduledRuns:
    outboundCredential:
      audience: kagent-egress
      tokenFile: /run/kagent-egress/token
      expirationSeconds: 3600
```

Kubernetes requires at least 600 seconds and may issue a different lifetime.
The actual token expiry governs an invocation, which may start partway through
a projected token's lifetime. Running tasks do not adopt rotated tokens; further
credential-dependent calls fail after expiry. Downstream services must verify
the outgoing audience and grant the intended authority. This separation applies
to scheduler credentials; ordinary caller-token passthrough retains the caller's
supplied token and its authority.

Runtime Actors use their verified atespace and Actor UID as principal identity.
Their Session binding carries the stored creator solely for data scoping, and
the runtime authorizer continues to restrict access to that Session and Agent.
`Principal` carries `ID`, authenticated `Claims`, and an optional `RuntimeBinding`.
Human and service-account principals have no runtime binding. The Agent is
resolved through the bound Session; there is no `Principal.Agent` field.
`UpstreamAuth` receives its destination Session ID separately from the caller.

## Why credential headers stay with integrations

ModelConfig and RemoteMCPServer already identify a connection endpoint. Keeping
credential headers beside that endpoint avoids requiring authors to repeat its host
in an EgressPolicy. EgressPolicy continues to describe additional destinations
and their credential headers, including arbitrary outbound HTTP requests.

All three sources resolve into the same internal destination/header/credential
binding and enforcement path. Integration destinations are derived from the
configured URL or resolved provider endpoint, using the existing origin rules.
An additional EgressPolicy entry is not required to restate that integration.

This means integration authors are trusted to configure the recipient and
credential headers together. If a deployment needs separate endpoint-authoring and
credential-administration permissions, that is an additional authorization
boundary; it is not a reason to require duplicated hosts in every deployment.

## Shared header shape

Use the same bounded `headers` list on each API. Every header requires a
`valueFrom` with exactly one typed source: a Secret reference or the invocation's
bearer token. Literal header values are outside this contract:

```yaml
apiVersion: api.kagent.dev/v1alpha3
kind: RemoteMCPServer
metadata:
  name: tickets
  namespace: support
spec:
  description: Search support tickets.
  protocol: STREAMABLE_HTTP
  url: https://tickets.example.com/mcp
  headers:
    - name: Authorization
      prefix: "Bearer "
      valueFrom:
        tokenPassthrough: {}
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata:
  name: support-model
  namespace: support
spec:
  provider: OpenAI
  model: support-chat
  openAI:
    baseUrl: https://models.example.com/v1
  headers:
    - name: Authorization
      prefix: "Bearer "
      valueFrom:
        secretKeyRef:
          name: model-access
          key: api-key
---
apiVersion: api.kagent.dev/v1alpha3
kind: EgressPolicy
metadata:
  name: issue-tracker-access
  namespace: support
spec:
  destinations:
    - origin: https://issues.example.com
      headers:
        - name: Authorization
          prefix: "Bearer "
          valueFrom:
            tokenPassthrough: {}
```

ModelConfig may also select tokenPassthrough; RemoteMCPServer and EgressPolicy may
select Secrets. A header list preserves MCP integrations that need more than one
credential header. ModelConfig `defaultHeaders` remain separate from credential
sources; RemoteMCPServer exposes only the shared credential header API.

The `valueFrom.tokenPassthrough: {}` selects the passthrough source without extra
parameters. Token passthrough supplies the bearer token value; prefix formats the
outgoing header. It does not nest an incoming `Bearer` scheme inside another one.
The source must come from authenticated invocation context, not an arbitrary
header, unverified agent ID, CRD creator, or stored Session creator.

Reject explicitly empty header lists, missing or multiple sources, invalid or
case-insensitively duplicate headers, and forbidden transport/routing headers.
Reuse existing Secret-reference, origin, and header validation. Conflicting
bindings for the same origin/header cannot both be effective; identical bindings
can merge. The exact update/revocation behavior must preserve existing safe
convergence rather than introduce an implicit credential override order.
Integration and EgressPolicy passthrough both require a live source grant. A
revision's expected headers do not preserve authority after that grant is removed.

Gateway bindings are scoped by origin and header, not by MCP tool name or URL
path. Different credentials for two clients at the same origin/header need a
different isolation boundary. Renaming an integration does not supply one.

## Passthrough semantics

| Invocation | Credential used by a passthrough binding |
| --- | --- |
| Human invokes an Agent | The authenticated human caller's token |
| Machine invokes an Agent | The authenticated machine caller's token |
| Scheduler invokes an Agent | The kagent service-account token supplied for the invocation |
| Invocation has no usable credential | An explicit authentication failure |

An invocation principal may differ from both the Session creator and the
executing Agent. The credential must represent that principal, with provenance
preserved when runtime machinery makes subsequent requests. The kind of subject
does not establish whether a credential is available.

Forward only to configured passthrough destinations. Those destinations must
trust the token's issuer and accept its audience and permissions. Passthrough
does not mint a token, change its audience, or grant additional scopes. If a
destination needs token exchange, that requires a separately configured and
implemented exchange path; it is not implicit fallback.

Missing, invalid, or expired credentials must not become anonymous requests or
silently select a Secret or another principal. A required model credential can
prevent invocation from starting. An optional MCP operation can surface its
authentication failure when selected. Do not reject a schedule merely because
its target declares passthrough or its recorded creator is a machine subject.

## Scheduled caller: the kagent service account

The scheduler is the caller, so its service-account credential is the initial
machine credential source. For a typical installation, the subject resembles:

```text
system:serviceaccount:kagent:kagent-controller
```

All schedules using that service account share its downstream identity and
permissions. Agent, schedule, execution, and Session IDs remain available for
attribution; they do not create separate downstream identities or restrictions.
This deliberately avoids needing a new issuer or per-Agent token minting for the
initial scheduling implementation.

Use a short-lived, audience-scoped token for the service account, through a
projected token or TokenRequest. Its receivers must accept the configured issuer
and audience. Do not automatically distribute the default Kubernetes API token:
it authenticates with the controller's Kubernetes permissions at the API server.
Audience selection, requested TTL, and any exchange belong to platform authentication
configuration, not a serviceAccountName field on Agent or ScheduledRun.

The authentication boundary must verify Kubernetes credentials, for example
through TokenReview with the intended audience or an appropriately configured
issuer/JWKS verifier. Reading `sub` from an unverified JWT is insufficient.
Trusted internal scheduler calls can use the same principal/credential contract
without requiring an HTTP loopback request.

Capture the verified outbound token for each invocation in memory. Never persist
it in the ScheduledRun, Session, execution payload, revision, or audit record.
An invocation relying on passthrough is limited by that token's remaining
lifetime. New invocations can use the current projection; expiration does not
renew or redispatch an already claimed A2A task.

Creating or modifying a schedule authorizes unattended use of this machine
authority through its configured integrations. Apply configuration and invocation
authorization accordingly. If schedules need distinct downstream permissions,
separate scheduling identities are a later extension rather than an assumed
property of the shared service account.

The existing runtime Actor JWT, bound to the `kagent-runtime` audience, remains
the credential for its own control-plane calls. It is not automatically the
invocation's passthrough credential.

## ScheduledRun and durable Session context

The CRD contains desired invocation configuration:

```yaml
apiVersion: api.kagent.dev/v1alpha3
kind: ScheduledRun
metadata:
  name: morning-triage
  namespace: support
spec:
  agentRef:
    name: ticket-assistant
  schedule: "0 9 * * 1-5"
  timeZone: UTC
  prompt: Summarize unassigned urgent tickets.
  paused: false
  executionTimeout: 15m
```

Start with same-namespace Agent references. Status reports observed generation and a Ready condition. Kubernetes owns desired schedule
configuration; PostgreSQL retains execution reservations, leases, Session links,
and history. A2A owns task identities, state, and transcripts.

Use schedule UID plus occurrence time for scheduled deduplication, and schedule
UID plus request ID for manual triggers. Capture accepted occurrence inputs and
target identity so retries cannot reinterpret later configuration. Preserve the
existing atomic Session reservation and dispatch-claim guarantees. A reconciled
database schedule projection is implementation state, not a second configuration
API.

Record the creating invocation's stable principal ID and trusted resource
associations. A scheduled Session records the scheduler's SA identity and its
execution link; an interactive Session records its authenticated creating caller.
Neither requires a synthetic human user or a resource-owner discriminator.

Creation and manual-trigger initiators are separate audit/provenance information.
Manually triggering a schedule still uses the scheduler's SA credential; a user
continuing a Session through a direct invocation supplies that new invocation's
own authenticated context.

Session services must apply principal-and-resource authorization to reads, listing
before pagination, continuation, lifecycle, shares, and checkpoints. Runtime
Actors remain limited to their bound Session and explicit service grants. Config
access does not automatically grant invocation or conversation access.

Memory retains its existing `(Agent namespace/name, creator principal ID)`
partition. Runtime requests resolve it from the verified Session binding, so
rotation, sharing, and a later caller do not select another partition. Schedules
using the same SA and Agent share memory. As with existing user memory, recreating
an Agent under the same name retains access to that principal's memory. Changing
to UID-based partitions would require a separate retention/migration contract.

Cron reservations read the live ScheduledRun through an uncached Kubernetes
reader and require its UID and generation to match the managed projection.
Schedules outside watched namespaces cannot reserve cron work. Missing, replaced,
or deleting sources retire their projections; transient API errors skip the
reservation without retiring the source. The store rechecks eligibility and
generation transactionally. A finalizer normally retires the projection before
deletion completes, while live-source checks cover forced or offline deletion.
Accepted executions and retained history survive. Accepted execution inputs
and Agent UID remain immutable; a replacement Agent cannot adopt old work. Recreated resource names must not
adopt state from old UIDs.

## Runtime and credential boundary

Compilation resolves shared credential headers into pinned destination bindings;
EgressPolicy supplies additional live grants. Go and Python model/MCP transports
use the authenticated caller token held in invocation-local context, independently
of the Session creator. No caller token is stored in Kubernetes objects, revisions,
Sessions, execution records, native process configuration, or a controller registry.
An invocation keeps its original token; later SA rotation affects only new
invocations. Token expiry is enforced by the destination, without renewal or
automatic redispatch.

Live header policy is fetched separately through the Actor-authenticated,
stateless `GET /runtime/egress` endpoint. It returns configuration and injection
placeholders, never caller credentials. Secret and Actor-token injection remain
at the Substrate gateway. Claude and Codex caller-token passthrough is unsupported;
their compilers reject integration bindings that require it.

See [credential injection](credential-injection.md) for transport requirements,
expiry, and discovery.

## Configuration, execution, and access

Kubernetes is the configuration API. ScheduledRunService CRUD wraps the actual
resource through the same `ResourceReference` + `StructuredObject` envelope used
by other CRDs, preserving UID and resourceVersion preconditions. The CRD spec is
the only configuration schema; there is no separate protobuf configuration model.
The scheduler stores a private JSON snapshot of that spec with the resource
version and resolved Agent UID. Indexed database columns own identity and timing.
This internal Go projection is written only by reconciliation and fenced by
UID/generation. Execution records and trigger/history RPCs remain protobuf APIs.
Status becomes Ready after synchronization. Invalid cron/time zones, a missing
Agent, or absent scheduler authentication disable new reservations and report a
condition; correcting the resource or Agent retries reconciliation. A disabled
projection rejects new manual executions as well as cron occurrences; pausing a
valid schedule only stops cron occurrences. Accepted requests remain replayable.

Manual triggers identify the schedule UID and reserve only the exact generation
whose target Agent the caller was authorized to invoke. A concurrent edit or
reconciliation requires a fresh authorization before a new reservation.
Accepted inputs include the prompt, deadline, Agent UID, schedule UID/generation,
and initiating principal. Cron occurrences deduplicate by UID/time and manual
triggers by UID/request ID. History uses the retained UID, so deletion and
recreation do not move old executions to a new resource.

Authorization treats configuration (`ScheduledRun`), execution
(`ScheduledRunInvocation`), and history (`ScheduledRunHistory`) separately.
Recorded namespace/name attributes continue to scope retained history after
resource deletion. An authorized operator can read or continue an SA-created
Session without assuming its creator identity. Session permissions remain
required; listing filters permissions before pagination. Read-only shares retain
their Session boundary and cannot invoke work. Shares remain managed by their
recorded creator. Runtime Actors use only their verified Session binding.

Checkpoints remain stored under the source Session's creator. Reading, listing,
creating, and forking them requires source Session read access and schedule history
permission, in addition to each checkpoint operation's permission. Renaming and
removing scheduled checkpoints requires the corresponding schedule invocation
permission. Retained execution provenance keeps those checks intact even after
idle expiration removes the source Session. Ordinary checkpoints remain private
to their owner. A fork belongs to its authenticated caller, with its own request
identity, and does not inherit the source's scheduled-execution provenance.

The scheduler's projected token audience is platform configuration. All schedules
in one installation share that SA's downstream authority. This implementation
adds neither a per-Agent token issuer nor a per-schedule service-account field.

## Alternatives superseded

| Earlier approach | Reason it was superseded |
| --- | --- |
| Reject scheduled targets that declare passthrough | Machines have tokens; validate credential availability and destination compatibility instead |
| Replace passthrough with mandatory human userDelegation | Conflates the propagation mechanism with the principal type |
| Require every integration's credential headers to move to EgressPolicy | Duplicates endpoint configuration without being necessary for common enforcement |
| Always present the target Agent's identity downstream | Passthrough intentionally presents the invocation caller, which may be a human or another machine |
| Mint a distinct Agent token for every schedule | The scheduler's existing SA provides the selected initial caller identity |
| Represent ownership as owner.user or owner.agent | A common principal ID and resource-aware policy express access without classifying owners as human or Agent |
| Persist separate issuer/sub fields or require SPIFFE IDs | The AuthProvider can normalize different authentication mechanisms behind a single opaque principal ID |

The selected scope does not require a user-grant CRD, per-schedule service-account
selection, a generic credential-source CRD, a SPIFFE integration, or a custom token
issuer. Additional mechanisms can be introduced when a concrete destination or
isolation requirement needs them.

## Compatibility and later extensions

This unreleased v1 API targets a clean installation. Schedule CRUD changes to Kubernetes
resources. There is no migration of creator-owned schedules or persisted token
state. The shared `headers` API supports `valueFrom.secretKeyRef`,
`valueFrom.tokenPassthrough`, and `valueFrom.actorToken`; literal values are
outside this contract. Actor tokens identify the executing runtime independently
of the scheduler or human caller and are minted by Substrate's gateway.

Token exchange, separate scheduling identities, and UID-based memory partitions need their own concrete
requirements and lifecycle contracts. They are not implicit behavior of token
passthrough or the CRD.
