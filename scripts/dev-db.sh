#!/usr/bin/env bash
# Creates the local development/test role and databases on a native PostgreSQL.
# Idempotent: safe to run repeatedly. Requires local superuser access through
# the "postgres" OS user (default on Debian/Ubuntu packages).
#
#   service postgresql start && scripts/dev-db.sh
#   export DATABASE_URL='postgres://ledger:ledger@127.0.0.1:5432/ledger_test?sslmode=disable'
set -euo pipefail

ROLE="${LEDGER_DB_ROLE:-ledger}"
PASSWORD="${LEDGER_DB_PASSWORD:-ledger}"
DATABASES=("${ROLE}" "${ROLE}_test")

psql_super() {
	if [ "$(id -un)" = "postgres" ]; then
		psql -v ON_ERROR_STOP=1 -X -q "$@"
	else
		su postgres -c "psql -v ON_ERROR_STOP=1 -X -q $(printf '%q ' "$@")"
	fi
}

psql_super -d postgres -c "DO \$\$
BEGIN
	IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${ROLE}') THEN
		CREATE ROLE ${ROLE} LOGIN PASSWORD '${PASSWORD}';
	END IF;
END
\$\$;"

for db in "${DATABASES[@]}"; do
	exists="$(psql_super -d postgres -tA -c "SELECT 1 FROM pg_database WHERE datname = '${db}'")"
	if [ "${exists}" != "1" ]; then
		psql_super -d postgres -c "CREATE DATABASE ${db} OWNER ${ROLE}"
		echo "created database ${db}"
	fi
done

echo "ready: postgres://${ROLE}:${PASSWORD}@127.0.0.1:5432/${ROLE}_test?sslmode=disable"
