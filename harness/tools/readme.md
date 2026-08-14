// -----------------------------------------------------------------------
// A note on adding more powerful tools (shell exec, network calls, writes,
// installing packages, etc.): treat anything that executes commands or
// mutates state as dangerous by default, since the MODEL - not you -
// decides when to call it mid-conversation. If you add a tool like that,
// strongly consider:
//   - a human confirmation step before it actually runs (easy to add in
//     Harness.runTool in harness.go)
//   - an allowlist of permitted commands/paths
//   - running it inside a sandbox or container, not directly on the host
// -----------------------------------------------------------------------