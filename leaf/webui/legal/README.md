# Discord application URLs

The four fields on the Discord Developer Portal (your app → **General Information**)
and what to put in each.

## Served by the webui (use these URLs)

| File | Route | Discord field |
|---|---|---|
| `terms-of-service.html` | `/legal/terms-of-service` | **Terms of Service URL** |
| `privacy-policy.html` | `/legal/privacy-policy` | **Privacy Policy URL** |

Both are embedded into the binary (`40-page.go`) and served **unauthenticated** by the
webui (`20-server.go`, alongside `/app.js`). With the webui at `agent.binserv.us`, the
public URLs are:

- `https://agent.binserv.us/legal/terms-of-service`
- `https://agent.binserv.us/legal/privacy-policy`

Paste those into the two Discord fields. They are embedded at build time, so edits to the
HTML need a rebuild + restart. Current values: operator **Binserv**, contact
**agent@binserv.us**, effective **2026-06-19**, retention **30 days**, providers
**OpenRouter and self-hosted models** — edit the HTML if any of these change.

(Written in English — Discord's app review is English.)

## The other two fields are NOT static pages — leave them blank

- **Interactions Endpoint URL** — *optional.* A live HTTPS webhook: Discord POSTs an
  Ed25519-signed `PING` and expects a signed `PONG`, then delivers interactions over HTTP
  instead of the Gateway. A static page **fails** that verification. bob receives
  everything over the **Gateway**, so leave this **empty** (only fill it if you build a
  real signature-verifying handler).

- **Linked Roles Verification URL** — *optional.* Only for Discord **Linked Roles** (an
  OAuth2 role-connection flow), which needs a real verification endpoint. bob doesn't use
  it — leave **empty**.
