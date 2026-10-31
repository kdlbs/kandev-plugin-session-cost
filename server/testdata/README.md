The Tokscale fixture uses the v4.15.1 CLI field shape with synthetic values.
Its two model groups total 300 tokens. The CLI emits no per-row `total` field.

The producer contract comes from [the pinned aggregation code](https://github.com/junhoyeo/tokscale/blob/v4.15.1/crates/tokscale-core/src/lib.rs#L3673)
and [the pinned JSON serializer](https://github.com/junhoyeo/tokscale/blob/v4.15.1/crates/tokscale-cli/src/main.rs#L2288).
Session grouping uses the exact source session ID and already-normalized exported model.
It merges provider labels and can merge clients while retaining the first contributor's client field.
Neither label proves uniform attribution.

`tokscale-session-model-v1` is the decomposition ID.
The member ID hashes that version, exact producer `sessionId`, and exact exported model.
The record key also uses the exact producer session and model.
Coverage and binding affect fact identity, but they do not change the member ID.
The canonical ACP ID only joins Host work and keys the per-coverage membership checkpoint.
Two rollout prefixes matching one ACP suffix therefore remain two producer members.

Membership checkpoints contain only version, bounded member hashes, and a quarantine flag.
The reservation and all report batches share one process lock.
Missing members block writes; restoring the prior set permits recovery.
Unknown pre-checkpoint legacy membership cannot be reconstructed from current labels or an archive scan.
Legacy compatibility remains part of final Host integration.
