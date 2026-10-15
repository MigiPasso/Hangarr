# Third-party notices

This file lists the third-party material that the repository contains or the build uses, with its license and source. The code written for Hangarr is under the MIT License (see [LICENSE](LICENSE)).

## Vendored files

### htmx

- Files: `internal/web/static/htmx.min.js`
- Version: 2.0.11 (`HTMX_VERSION` in `Makefile`; the file starts with `version:"2.0.11"`)
- License: 0BSD
- Source: https://github.com/bigskysoftware/htmx
- Downloaded from unpkg by `make vendor-assets`.

### Fonts

The font files are in `internal/web/static/fonts/`. All families are licensed under the SIL Open Font License, Version 1.1. The full license text and the copyright notice of each family are in the file listed below.

| Family | Font files | License file | Copyright line (from the license file) |
|---|---|---|---|
| Inter | `inter-400.woff2`, `inter-500.woff2`, `inter-600.woff2`, `inter-700.woff2` | `fonts/OFL-inter.txt` | Copyright 2016 The Inter Project Authors (https://github.com/rsms/inter) |
| Geist | `geist-400.woff2`, `geist-600.woff2`, `geist-700.woff2` | `fonts/OFL-geist.txt` | (C) 2023 Vercel, made in collaboration with basement.studio |
| Nunito | `nunito-400.woff2`, `nunito-600.woff2`, `nunito-800.woff2` | `fonts/OFL-nunito.txt` | Copyright 2014 The Nunito Project Authors (https://github.com/googlefonts/nunito) |
| Chakra Petch | `chakra-petch-400.woff2`, `chakra-petch-600.woff2`, `chakra-petch-700.woff2` | `fonts/OFL-chakra-petch.txt` | Copyright 2018 The Chakra Petch Project Authors (https://github.com/m4rc1e/Chakra-Petch.git) |
| Share Tech Mono | `share-tech-mono-400.woff2` | `fonts/OFL-share-tech-mono.txt` | Copyright (c) 2012, Carrois Type Design, Ralph du Carrois (www.carrois.com post@carrois.com), with Reserved Font Name 'Share' |

### Common password list

- File: `internal/auth/common-passwords.txt`. Its first line records the source.
- Source: SecLists, `Passwords/Common-Credentials/10k-most-common.txt` (`COMMON_PASSWORDS_URL` in `Makefile`)
- License: MIT, Copyright (c) 2018 Daniel Miessler. Full text: `internal/auth/LICENSE-SecLists.txt`.
- Downloaded by `make vendor-assets`.

## Test data and images

- The synthetic scenario in `testdata/` is written for this project (see `testdata/README.md`).
- `testdata/posters.json` and `testdata/fanarts.json` contain URLs only. No image is stored in the repository.
- The dev mock downloads the images at run time, only when `MOCK_REAL_POSTERS` is enabled. `compose.dev.yml` enables it by default; `make e2e` disables it. The URLs point to IMDb, TVmaze and TMDB. The images belong to their providers and are not redistributed here.
- `testdata/captured/` is not part of the repository.

## Go modules

- The modules are declared in `go.mod` and their checksums are in `go.sum`. They are not vendored: the Go toolchain downloads them inside the containers.
- Direct requirements: `golang.org/x/crypto`, `golang.org/x/sync`, `golang.org/x/text`, `modernc.org/sqlite`. The other modules listed in `go.mod` are indirect dependencies.
- Each module is distributed under the license published by its authors. This file does not restate them; consult each module's license file.

## Container images

The build, test and lab tooling runs from container images pinned in `Dockerfile`, `compose.dev.yml` and `deploy/jellyfin-lab/compose.yml`. They are not part of the repository, and each keeps the license of its upstream project:

- `golang` (Go toolchain), `golangci/golangci-lint` (lint), `air` (installed in the dev image);
- `gcr.io/distroless/static-debian12` (base of the runtime and mock images);
- `mcr.microsoft.com/playwright` (end-to-end tests);
- `curlimages/curl` (`make vendor-assets`), `alpine` (`make icons`, Jellyfin lab);
- `jellyfin/jellyfin` (Jellyfin lab, `make jellyfin-lab`).
