# AGENTS

## Backup integrity

Local-copy daemons (`onlineapi`, `vacuum`, `internal/localcopy`) do not run `PRAGMA integrity_check`. They only snapshot and atomically promote. Verification is the consumer's job on the other machine after download (`rsync`/`sftp`/`replica` clients). Do not re-add integrity checks to the daemons.
