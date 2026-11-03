# Microsoft Teams integration (xsense fork)

This branch adds a Microsoft Teams channel to Multica. It is a downstream
addition maintained in `xsense/multica` (`teams-integration`), tracking
upstream tags. Deployment and Azure setup live in `xsense/multica-eks`
(README, section 8).

## How it works

```
Teams ──► Bot Framework Connector ──POST /api/webhooks/teams──► teams.Webhook
                                                                 │ verify JWT (iss, aud=App ID, RS256,
                                                                 │ msteams endorsement, serviceUrl claim)
                                                                 │ verify tenant == installation tenant
                                                                 ▼
                                                     engine.Router.Handle  (shared pipeline:
                                                     identity → dedup → session → /issue → run)
agent finishes ─► EventChatDone ─► teams.Outbound ─► Connector REST (POST activity, markdown)
```

* One Azure Bot (single-tenant Entra app) per agent, keyed by App ID in
  `channel_installation.config->>'app_id'`. Client secret encrypted with
  `MULTICA_TEAMS_SECRET_KEY` (base64, 32 bytes).
* Sessions: personal chat and group chat = one session each; a channel
  thread (`19:…;messageid=<root>`) = one session.
* Identity: by default a Teams user links through the one-time bind link
  (`/teams/bind`). Opt-in: with `MULTICA_TEAMS_AUTO_BIND_DOMAINS` set, a
  non-guest whose directory email is on a listed (tenant-verified) domain is
  bound to the Multica user with that email on first message.
* Typing indicator refreshed every 3 s while the run is going (max 15 min).
* `/issue` issues carry `origin_id` = chat session but no `origin_type`:
  `issue_origin_type_check` is rewritten by upstream whenever it adds a
  channel, and a fork value there would break the next upgrade.
* No database migration, so rolling back to an upstream image only needs
  the Teams installations disconnected first.

## Upstream files touched (resolve these on rebase)

Backend
* `server/cmd/server/router.go` — Teams wiring block after Telegram, public
  route `POST /api/webhooks/teams`, workspace routes, bind redeem route
* `server/internal/handler/handler.go` — `Teams*` fields
* `server/pkg/protocol/events.go` — `teams_installation:*` events

Frontend
* `packages/core/api/client.ts`, `api/schemas.ts`, `types/index.ts`,
  `package.json` (exports), `realtime/use-realtime-sync.ts`
* `packages/views/settings/components/channels-tab.tsx`,
  `integration-channel-icon.tsx`, `settings-navigation.ts`
* `packages/views/agents/components/tabs/integrations-tab.tsx` (+ test)
* `packages/views/settings/components/channels-tab.test.tsx`
* `packages/views/locales/*/{settings,common}.json` (`teams`, `teams_bind`)
* `packages/views/package.json` (exports)

Everything else is new files: `server/internal/integrations/teams/`,
`server/internal/handler/teams.go`, `packages/core/teams/`,
`packages/core/types/teams.ts`, `packages/views/teams/`,
`packages/views/settings/components/teams-{tab,mark}.tsx`,
`apps/web/app/teams/bind/page.tsx`, `.github/workflows/xsense-teams.yml`.

## Release

```
git fetch upstream --tags
git rebase <new upstream tag> teams-integration   # fix conflicts in the files above
git push --force-with-lease origin teams-integration
git tag xsense-<new tag>-teams.1 && git push origin xsense-<new tag>-teams.1
```

The `xsense teams` workflow builds, vets, tests and publishes
`ghcr.io/xsense/multica-{backend,web}:<tag>` for amd64 and arm64.

## Not in v1

* Files and images sent in Teams (needs Graph consent for SharePoint/OneDrive).
* Streaming partial answers (Teams throttles edits and marks them "Edited").
* Adaptive cards / message extensions.
