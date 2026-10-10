# TODO

- [ ] Publication automatique de l'image sur GHCR : workflow Forgejo Actions déclenché par un tag `v*`
  (build amd64 + arm64, push `:<version>` et `:latest`). En attendant : `make image PUSH=1` (README, "Publishing a release").
