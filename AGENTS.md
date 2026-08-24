# AGENTS

## Backup integrity

Local-copy daemons (`onlineapi`, `vacuum`, `internal/localcopy`) do not run `PRAGMA integrity_check`. They only snapshot and atomically promote. Verification is the consumer's job on the other machine after download (`rsync`/`sftp`/`replica` clients). Do not re-add integrity checks to the daemons.

## Landlock

The sqlite-rsync replica daemon runs without landlock confinement. The sqlite-rsync protocol is label-addressed: no filesystem path ever crosses the wire, and each sync applies the received stream to exactly one pre-configured replica file, so a compromised or buggy origin cannot nominate a read or write target. Filesystem confinement would guard a bug class the protocol shape does not expose, while costing a Linux 5.13+ kernel requirement and portability; the secret it could never protect anyway — the SSH private key — lives in memory, and a memory-safety bug in the stream parser is out of scope for confinement regardless. Do not re-add landlock to the replica command; the landlock package remains for commands whose protocols do expose paths (the rsync/sftp clients).

## Markdown headings

Do not put markdown links inside headings. The local renderer slugifies the raw heading markup, so a heading like `## foo ([`bar`](https://…))` generates an anchor that contains the whole URL (`foo-barhttpsgithubcom…`) and matches nothing in the content table; GitHub strips the link markup before slugging and generates a different anchor. Keep headings plain text — inline code in backticks is fine, `[text](url)` is not — so the auto-generated anchor is identical in every renderer and the content table links always resolve.
