# ScheduledRun and caller identity: design decision

Decision recorded: 2026-10-24. The principal and service-account authentication
foundation and shared credential header API are implemented. The ScheduledRun CRD
sections describe the remaining dependent change.

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

Kubernetes authentication uses TokenReview with `server.auth.serviceAccount.audience`.
It maps the verified SA UID under the configured stable cluster `authority` into
an opaque ID. Token rotation preserves the ID; deleting and recreating an SA
changes it. Configure distinct authorities for distinct Kubernetes authentication
sources sharing a store, and keep the authority stable across controller restarts.

Helm projects a one-hour `kagent-invocation` token separately from the Kubernetes
API credential and grants TokenReview access. Kubelet rotates the projection.
The scheduler's credential source reads and verifies it on every use, rejects a
changed SA UID, and returns errors without token material. External SA callers
use the same verifier; their supplied credential is reverified before forwarding.
Custom authentication providers retain their own forwarding contract.

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
Audience selection, renewal, and any exchange belong to platform authentication
configuration, not a serviceAccountName field on Agent or ScheduledRun.

The authentication boundary must verify Kubernetes credentials, for example
through TokenReview with the intended audience or an appropriately configured
issuer/JWKS verifier. Reading `sub` from an unverified JWT is insufficient.
Trusted internal scheduler calls can use the same principal/credential contract
without requiring an HTTP loopback request.

Bind a refreshable credential source to execution rather than persisting a
bearer token in the ScheduledRun, Session, execution payload, revision, or audit
record. Read projected token rotation or renew TokenRequests as required. A
running execution must not remain stuck with the token captured when its first
request started. Refresh must retain the intended subject and audience, and must
not redispatch an already claimed A2A task.

Creating or modifying a schedule authorizes unattended use of this machine
authority through its configured integrations. Apply configuration and invocation
authorization accordingly. If schedules need distinct downstream permissions,
separate scheduling identities are a later extension rather than an assumed
property of the shared service account.

The existing runtime Actor JWT, bound to the `kagent-runtime` audience, remains
the credential for its own control-plane calls. It is not automatically the
invocation's passthrough credential.

## ScheduledRun and durable Session context

The proposed CRD contains desired invocation configuration:

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

Start with same-namespace Agent references. Status reports observed generation,
conditions, and bounded scheduling summaries. Kubernetes owns desired schedule
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

The working deletion semantics are that schedule deletion stops new reservations
once reconciled, while accepted executions and retained history survive. Define
this asynchronous boundary in the CRD contract. Recreated resource names must not
adopt state from old UIDs.

## Runtime and credential boundary

Compilation resolves the shared credential header configuration into explicit
destination bindings. Credential acquisition and propagation use the verified
invocation context. The gateway remains the preferred place to inject credentials
without exposing values to guest runtimes. A common API must not imply that every
harness already implements these paths.

Invocation credentials cannot be cached solely by Session creator, Agent, or
origin. A later invocation can have a different caller. Credential handles, MCP
connections, background streams, and paused/resumed work must preserve the
originating invocation's authority rather than inherit the most recent token.
Checkpoint/fork operations do not create permission to reuse a caller credential.
How that binding is carried through the gateway and across restarts remains an
implementation decision.

Current Substrate injection replaces a header only when the client supplies it.
Existing placeholders can continue to satisfy that contract where supported.
Adding or overwriting a missing policy-managed header requires a Substrate change;
do not claim that behavior exists today. The shared credential header API alone does
not solve invocation-scoped credential routing or token renewal.

Authenticated tool discovery also needs an explicit caller context. A global
discovery result cannot assume all human and machine subjects see the same tools.
Controller-originated requests must not accidentally substitute the controller
credential for an unrelated invocation's credential.

## Current implementation and affected boundaries

The following are current limitations, not implemented parts of this decision:

| Boundary | Current behavior | Required direction |
| --- | --- | --- |
| [ScheduledRun protobuf](../../proto/kagent/api/v1/scheduled_runs.proto) and [execution store](../../go/core/internal/database/scheduled_runs.go) | Schedule configuration is creator-owned PostgreSQL state | CRD configuration with retained durable execution machinery |
| [Scheduler controller](../../go/core/internal/controller/scheduledrun/controller.go) | Uses an empty ControlPlaneSession | Explicit machine invocation principal and refreshable SA credential |
| [Authentication types](../../go/core/pkg/auth/auth.go) and [runtime binding](../../go/core/internal/runtimeauth/provider.go) | Actor binding requires Session.creator and exposes it through User | Opaque principal ID, authenticated attributes, and independent runtime binding |
| [Session service](../../go/core/internal/service/session/service.go) and [memory service](../../go/core/internal/service/memory/service.go) | Storage access and memory are coupled to user/creator fields | Principal-and-resource authorization and a stable data partition |
| [Credential compiler](../../go/core/internal/translator/credentials.go) | Separate API-key, MCP header, and passthrough fields | Shared header types and canonical destination bindings |
| [Gateway credential injection](credential-injection.md) | Passthrough support differs by harness; built-in injection uses placeholders | Verified invocation credential routing and supported harness behavior |

The proxy authenticator assumes a trusted upstream validation boundary; parsing
its claims is not a new service-account verifier. Existing TokenReview use for
protected controller metrics does not establish SA authentication for the public
Agent invocation path.

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

## Details still to finalize

- Exact shared header schema, provider-specific header defaults, field
  replacement/versioning, and compatibility scope.
- SA token audience configuration, projected-token versus TokenRequest delivery,
  validation ownership, and renewal behavior across controller restart.
- Invocation credential binding through gateway injection, cached MCP sessions,
  asynchronous work, continuation, suspension, and resume.
- Provider normalization and identity-recreation semantics, memory partition,
  and historical access after deletion. The creator field remains one principal ID.
- Trusted resource attributes available to the authorizer and the default policy
  for private versus scheduled Sessions; caller attributes alone do not define access.
- CRD deletion/accepted-execution semantics, status fields, and configuration
  synchronization guarantees.
- Which concrete token-exchange and gateway extensions, if any, are necessary
  for the first supported deployment. No proposed field should advertise an
  unimplemented credential mechanism.
