# MAX certificate trust

MAX requires the Ministry of Digital Development certificate for
`platform-api2.max.ru`: [official API documentation](https://dev.max.ru/docs-api).

`russian_trusted_root_ca.pem` was downloaded over verified HTTPS on 4 October
2026 from the [Gosuslugi certificate distribution](https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt).
The [Gosuslugi certificate page](https://www.gosuslugi.ru/crt) describes installation.

- Subject and issuer: Russian Trusted Root CA, The Ministry of Digital Development and Communications.
- Validity: 1 March 2022 – 27 February 2032.
- Certificate SHA-256: `d26d2d0231b7c39f92cc738512ba54103519e4405d68b5bd703e9788ca8ecf31`.
- PEM file SHA-256: `936a43fea6e8e525bcc0f81acd9c3d21b4fc4b9b68acea7906d698005afc6504`.

The root is embedded and appended to a private MAX HTTP client's system roots.
It is not installed into the operating system or added to other providers'
clients. TLS certificate and hostname checks remain enabled. An optional
`MAX_CA_CERT_FILE` adds an operator-provided PEM bundle to the same client.

Before updating the root, download it from the official source over verified
HTTPS, inspect its CA constraints, validity and fingerprint, and update the
fingerprint regression test. Verify an unauthenticated request to MAX from the
runtime container: HTTP 401 demonstrates successful TLS without using a bot
token or changing MAX data.
