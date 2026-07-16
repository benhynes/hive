# Teams

Hive teams are repeatable groups of spawn profiles. Put manifests in
`~/.hive/teams/<name>.yaml`, then manage the whole group with:

```sh
hive team up zlt
hive team status zlt
hive team down zlt
```

`team up` launches members concurrently, waits for runtime readiness, and
replaces existing registrations and sessions by default. Use `--no-replace`
to make an existing member a conflict instead.

```yaml
name: zlt
host: debian-dev # optional; defaults to the local host
members:
  - name: zlt-lead
    profile: zlt-lead
  - name: zlt-codex
    profile: zlt-codex
  - name: zlt-claude
    profile: zlt-claude
  - name: zlt-reviewer
    profile: zlt-reviewer
```

Each member may also set `cwd`, `grant_control`, `nudge`, or `persist`.
Transcripts are retained when members are replaced or stopped.

For unattended Codex profiles, disable the startup update prompt explicitly:

```json
{
  "runtime_setup": {
    "type": "codex",
    "auth_source": "~/.codex/auth.json",
    "check_for_updates": false
  }
}
```
