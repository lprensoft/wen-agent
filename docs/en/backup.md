# Backup and restore

[← Docs home](README.md)　·　[← Back to the project README](../../README.en.md)　·　[中文](../backup.md)　·　English

`wen export` packs the data the program maintains in the configuration directory into an archive, and `wen import` restores it into an empty directory.

## Backing up

```bash
wen export
wen export -o ~/backups/wen-20261106.tar.gz
wen export -c /srv/wen/config.yaml -o /backup/wen.tar.gz
```

By default it writes `wen-backup-<date>-<time>.tar.gz` in the current directory with mode 0600.

**Stop the service first.** Session files are still being appended to, so an online snapshot can end mid-line. The command refuses while the service is running; add `--force` if you really need an online snapshot, and accept that it may be inconsistent. `--force` also allows overwriting an existing output file.

It includes:

| Path | Contents |
|---|---|
| `config.yaml` | Startup configuration (under the actual file name given with `-c`) |
| `models.json` | Providers, keys and the current model |
| `plugins.state.json` | Plugin switches and settings saved in the UI |
| `auth.json` | The PBKDF2-derived access password |
| `sessions/` | Every conversation |
| `plugins/` | Memories, archives, heartbeat state, channel tokens and so on |
| `bundles/` | External plugin bundles |

It does not include:

- `wen.lock` (the running-instance record);
- symbolic links and special files;
- a session directory outside the configuration directory (`session.dir` pointing elsewhere prints a note);
- project memories outside the configuration directory: when the `memory` plugin uses `scope=project` and the working directory is elsewhere, it prints a note and skips them; when the working directory is inside the configuration directory, the per-domain `memories-<tag>` libraries are included as well.
- logs, build outputs and empty directories.

**The backup contains sensitive data.** `models.json` holds plaintext API keys, plugin state holds channel credentials, `auth.json` holds the derived password, and the sessions hold every conversation. The file mode is 0600, but encrypt it or store it somewhere safe anyway; do not commit it to a repository.

## Restoring

```bash
wen import ~/backups/wen.tar.gz
wen import ~/backups/wen.tar.gz -c /srv/wen/config.yaml
```

The target directory is the directory holding the config file given with `-c`. It **must be empty or not exist yet**: nothing is merged and nothing existing is overwritten. To restore over an existing directory, move the old one aside first.

It likewise refuses while the service is running. After restoring:

```bash
wen status -c /srv/wen/config.yaml
wen serve -c /srv/wen/config.yaml
```

A backup from a different program version only produces a note, not a refusal; an archive format newer than this program supports is rejected. Restore checks that `models.json` and `plugins.state.json` are valid JSON and that the file count and total bytes match the manifest, so a corrupt or incomplete archive never lands in the target directory.

## Archive layout

```text
manifest.json
data/config.yaml
data/models.json
data/plugins.state.json
data/auth.json
data/sessions/...
data/plugins/...
data/bundles/...
```

`manifest.json` records the format version, program version, creation time, source platform, config file name, file count and total bytes. Import accepts relative paths only and rejects `..`, absolute paths, backslashes, symbolic links and hard links. One archive may contain at most 200,000 entries and 4 GiB. All validation and extraction happen in a temporary directory, so a failed import never pollutes the target.
