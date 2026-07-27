# Third-Party Notices

## Mieru

daonode embeds the Mieru server API from:

- Project: https://github.com/enfein/mieru
- Module: `github.com/enfein/mieru/v3`
- Version: `v3.34.1`
- License: GNU General Public License v3.0

The GPL-3.0 license text is included in `LICENSE-GPL-3.0`.

The original daonode/v2node-derived source files remain under their existing
MPL-2.0 terms where applicable. A distributed daonode binary that links Mieru
must be distributed in compliance with GPL-3.0.

## NaiveProxy server

daonode embeds the padding-enabled forward proxy designated by NaiveProxy's
official server setup:

- Project: https://github.com/klzgrad/forwardproxy/tree/naive
- Module replacement: `github.com/caddyserver/forwardproxy` -> `github.com/klzgrad/forwardproxy`
- Commit: `d62c80d3dd2c706b6b87579844d2397bddd18317`
- License: Apache License 2.0

The surrounding Caddy and quic-go modules retain their respective upstream
licenses.

## Juicity server

daonode's `core/juicity` adapter is derived from and interoperates with the
official Juicity v0.5.0 server implementation:

- Project: https://github.com/juicity/juicity
- Module: `github.com/juicity/juicity`
- Version: `v0.5.0`
- License: GNU Affero General Public License v3.0

The adapter retains Juicity's QUIC authentication and TCP/UDP wire formats,
while adding DaoNode lifecycle, hot-user and accounting hooks. A distributed
daonode binary containing this adapter must be distributed in compliance with
AGPL-3.0. The license text is included in `LICENSE-AGPL-3.0`.
