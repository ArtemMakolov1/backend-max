# Browser API contract

`openapi.yaml` is the canonical contract shared by the Go API and the React/Astro
frontend. It describes the public `/api/v1` routes used by the browser; internal
compatibility aliases are intentionally excluded.

The backend test verifies that every canonical browser route and method remains
present. The frontend generates TypeScript declarations from this file with:

```bash
npm run generate:api
```

`npm run check:api` is a CI gate: it regenerates the declarations and fails when
the committed TypeScript contract is stale. When both repositories change in one
release, publish the backend contract first and then the frontend consumer.

Do not edit `frontend/src/lib/generated/api-contract.d.ts` by hand.
