# Production strategy review — 2026-10-08

Recommendation: retain the Center/direct-node architecture, but revise the admission, ownership and recovery contracts before M1. Fleet counts remain estimates until M0 measurements.

Verified findings from proposal and current code:

- P1 — Authentication prerequisite: the referenced licensing design claims a full Ed25519 signature is 32 bytes. Standard Ed25519 signatures are 64 bytes; a 16-byte payload plus signature is 80 bytes, or 107 unpadded Base64URL characters. Resolve issuer format and identity binding before implementing admission. Assignment request also needs authenticated camera identity, and tokens must bind role and URL identity.
- P1 — Player authorization (plan 305–308): checking only request.id leaves answer and other arbitrary forwarded messages unchecked. Current readPump forwards every non-bye type by destination id. Specify authorization for every message and destination, plus camera-to-viewer session binding.
- P1 — Fallback contract (plan 294–335): one five-minute node-bound token cannot authenticate at alternative nodes or after 24-hour cache reuse. Define endpoint-specific credentials and a secure Center-outage reconnect mechanism. A cached URL alone is insufficient.
- P1 — Registry ownership (plan 338–352): specify shared storage before M2, atomic epoch allocation and compare-and-set registration, conditional refresh/delete, stale-owner fencing, and partition behavior. Higher epoch alone does not stop a partitioned old node routing locally. Define how legacy devices obtain an epoch.
- P1 — Legacy rollout (plan 145–159, 191–193): a Center-only LB cannot also serve persistent legacy signaling without combined-role backends or an explicit forwarding path. M2 precedes the M3 compatibility work; move compatibility into M2 entry criteria and budget persistent legacy LB connections.
- P1 — Regional recovery (plan 107–109, 371–373): claimed 100k emergency capacity conflicts with L < 0.80 admission. Base case after AP loss has 15 survivors: 1.3M / 15 = 86.7k connections/node, above the 80k cap. Specify and test emergency admission or provision sufficient usable capacity at the normal cap. CPU/memory thresholds may lower usable capacity further.
- P2 — TURN recovery (failure table): alternative ICE entries help new negotiations; an established allocation on a failed TURN server is not migrated by shared credentials. Specify ICE restart/peer recreation, refreshed credentials and recovery tests for an active forced-relay stream. TURN selection also needs bandwidth/allocation load rather than signaling connection limits.

Independent review: gpt-6-luna with low reasoning identified fallback, fencing, legacy LB, message authorization and shared-store gaps. Primary reviewer checked each against the cited plan and signaling-server.go. No runtime/load/chaos tests were run; this is a design review.

Sources checked:
- https://pkg.go.dev/crypto/ed25519 (SignatureSize = 64)
- https://docs.digitalocean.com/products/networking/load-balancers/how-to/scale/ (10k simultaneous connections and 250 new SSL connections/s per regional LB node)
- https://pkg.go.dev/github.com/pion/turn/v4 (TLS listener, REST auth, permission and quota hooks)

Blind spot: healthy WebSockets and successful assignment do not establish playable video or recovery of active media sessions. Add end-to-end first-frame and forced-relay recovery criteria.

Confidence: 9/10 for identified contract contradictions; fleet sizing and throughput need measurements. An external existing auth/registry protocol could resolve some omissions, but it is not specified here.
