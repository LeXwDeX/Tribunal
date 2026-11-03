# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub Security Advisories:

<https://github.com/LeXwDeX/Tribunal/security/advisories/new>

Do not open a public Issue for a vulnerability, and do not include tokens, keys,
`.env` contents, request bodies or personal data in a report. Include the affected
version or commit, a minimal reproduction, the impact and any known mitigation.

## Scope

The supported line is the current `main` and the latest published minor release.
Reports about the review DAG, GitLab publishing, workspace isolation, secret
handling, authentication, CSRF, or webhook verification are in scope. Vulnerabilities
in third-party services, upstream GitLab or model providers should be reported to
those projects; mention them here only if Tribunal's integration adds exposure.
