#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tout tourne sans Docker, avec les versions d'outils de mise.toml (installées par mise).
# Usage : ./scripts/test.sh [paquets…]   tests Go (ex. ./internal/config/...) ; sans argument : tests, qualité (gofmt + vet), puis audit
#         ./scripts/test.sh types         qualité : gofmt + go vet
#         ./scripts/test.sh audit         govulncheck
# Le test d'intégration (test/integration.sh, docker:dind) n'est pas lancé ici : voir CLAUDE.md.
command -v mise >/dev/null || { echo "mise est requis (https://mise.jdx.dev) : voir CONSIGNES-DEV.md" >&2; exit 1; }
mise trust -q >/dev/null 2>&1 || true
mise install -q
m() { mise exec -- "$@"; }
export CGO_ENABLED=0

types() {
  local unformatted
  unformatted="$(m gofmt -l .)"
  if [ -n "$unformatted" ]; then echo "gofmt : fichiers à formater :" >&2; echo "$unformatted" >&2; return 1; fi
  m go vet ./... && echo "gofmt + go vet : OK"
}
audit() { m govulncheck ./...; }

case "${1:-}" in
  audit) audit; exit $? ;;
  types) types; exit $? ;;
esac

if [ $# -eq 0 ]; then
  m go test ./...
  types
  audit || { echo "⚠ govulncheck : vulnérabilités à corriger (voir ci-dessus)" >&2; [ "${AUDIT_STRICT:-0}" != 1 ]; }
else
  m go test "$@"
fi
