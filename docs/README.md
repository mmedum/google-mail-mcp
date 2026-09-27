# Documentation

| File | For | Purpose |
|---|---|---|
| [`gcp-setup.md`](gcp-setup.md) | first run | The Cloud project, consent screen, scopes and OAuth client. Its scope lists are generated from the code. |
| [`configuration.md`](configuration.md) | running it | Every setting, where state is stored, scopes per mode, `status --json`. |
| [`runbook.md`](runbook.md) | when something surprises you | Weekly token expiry, missing scopes, revoking, suspected exposure, rate limits, an ambiguous send. |
| [`security.md`](security.md) | deciding whether to run it | What it talks to, what the token can do, how mail is treated as untrusted, what is logged, what it refuses. |
| [`architecture.md`](architecture.md) | changing it | The design, the platform facts behind it, a verdict on every API method, the phases and the evidence log. |
| [`development.md`](development.md) | changing it | `make check`, what each gate holds, the live driver, adding a tool. |
| [`release.md`](release.md) | release day | The tag, what to check after it, and the recovery for each step no rehearsal reaches. |

The reporting policy is [`../SECURITY.md`](../SECURITY.md); the quick
start is [`../README.md`](../README.md).
