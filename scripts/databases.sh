#!/bin/sh
# Starts (or stops) the PostgreSQL server used by tests and benchmarks.
# Tests create and drop their own databases on it. SQLite and Turso need
# no server.
set -eu
DOCKER=${DOCKER:-docker}
PG=atomicfga-postgres

case "${1:-up}" in
up)
	if ! $DOCKER container inspect $PG >/dev/null 2>&1; then
		$DOCKER run -d --rm --name $PG -p 55432:5432 -e POSTGRES_PASSWORD=atomicfga \
			postgres:17 -c max_connections=500 >/dev/null
	fi
	i=0
	until $DOCKER exec $PG pg_isready -U postgres >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 120 ] || { echo "postgres did not become ready" >&2; exit 1; }
		sleep 1
	done
	echo "postgres: postgres://postgres:atomicfga@localhost:55432/postgres?sslmode=disable"
	;;
down)
	$DOCKER rm -f $PG >/dev/null 2>&1 || true
	;;
*)
	echo "usage: $0 [up|down]" >&2
	exit 2
	;;
esac
