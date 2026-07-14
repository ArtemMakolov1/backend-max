# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through a
[GitHub security advisory](https://github.com/ArtemMakolov1/backend-max/security/advisories/new).
Do not publish credentials, access tokens, personal data, or exploit details in a public issue.

Include the affected endpoint or component, reproduction steps, impact, and a minimal proof of
concept when it is safe to share. You can expect an acknowledgement after the report is reviewed.

## Supported version

Security fixes are made on the `main` branch. Deployments should use the latest successful build
from that branch and provide all runtime secrets through environment variables or a secret store.
