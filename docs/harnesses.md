# Harness integration

Hive installs the common local tooling surface into every supported harness:

```sh
hive harness sync
hive harness status
```

The current adapters support Codex and Claude Code. Each adapter:

- detects the harness executable from `PATH`;
- registers `hive mcp` globally at user scope;
- discovers Forcefield client settings from installed Hive spawn profiles and
  registers `ff mcp` globally when available;
- merges a small managed guidance block into the harness's global instruction
  file so agents know these are tool-only MCP servers and do not probe guessed
  HTTP ports.

`hive team up` runs the sync automatically for a local team. Use
`--no-harness-sync` only when an administrator manages harness configuration
through another mechanism.

Adding another harness requires one adapter describing its executable, global
MCP registration commands, status command, and global guidance path. Hive and
Forcefield themselves remain unchanged.
