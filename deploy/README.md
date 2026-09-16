# Tailnet deployment

This fork is intended to run as a loopback-only service behind Tailscale Serve.
Provider credentials stay on the host, while clients receive independent router
API keys.

## Security boundary

- Keep the application bound to `127.0.0.1`.
- Start it with `--local-model`; remote model catalogs are disabled by default.
- Keep remote management, its downloaded control panel, discovery, plugins, Git
  storage, and request logging disabled.
- Store the configuration with mode `0600` and the auth directory with mode
  `0700`.
- Expose the loopback listener through Tailscale Serve and restrict it to the
  intended users and devices in the Tailnet policy.
- Use authorization headers. Do not place router or provider keys in URLs.

On Omarchy, T3 already owns the root handler on Tailnet HTTPS port 443. Add the
router as a separate path without replacing T3:

```sh
tailscale serve --bg --yes --https=443 --set-path=/ai-router http://127.0.0.1:8317
```

Clients use `https://omarchy.tail416faa.ts.net/ai-router` as the base URL.

## Provider boundary

API-backed providers such as Venice can be configured under
`openai-compatibility`. Claude subscription traffic is different: it must remain
inside the official Claude Code / Agent SDK harness. Do not configure a generic
Anthropic-compatible endpoint as a substitute for that harness.

## Native service

Build a pinned revision and place the binary under a versioned directory in
`~/.local/share/ai-router/releases/`. Atomically update the `current` symlink,
then restart the user service. Keep at least one previous release for rollback.

The example service applies a restrictive umask, filesystem sandbox, memory
limit, task limit, and restart policy. Review its paths if deploying under a user
other than `will`.
