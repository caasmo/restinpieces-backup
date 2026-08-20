# sqlite-rsync-origin

Run the origin daemon from the repo root:

    go run ./cmd/sqlite-rsync/origin/daemon -config /path/to/config.toml

The config file holds the [backup] section of the restinpieces
application configuration — the same document ripc scaffolds:

    [backup.files.app_db]
    source_path = "/path/to/db"

## Running without the restinpieces dependency

The standalone main imports restinpieces only for the backup section
shape: `config.Backup` and `config.BackupFile`. To drop the
dependency, copy those two structs from restinpieces into your own
main and replace the references.

There is no validation in the standalone path: the daemon only checks
that at least one file is configured. To get validation, copy
`config.ValidateBackup` (restinpieces `config/config_validate.go`)
and call it after unmarshal.