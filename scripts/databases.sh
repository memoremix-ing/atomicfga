#!/bin/sh
# Starts (or stops) the PostgreSQL and MySQL servers used by tests and
# benchmarks. Tests create and drop their own databases on these servers.
set -eu
DOCKER=${DOCKER:-docker}
PG=atomicfga-postgres
MY=atomicfga-mysql

case "${1:-up}" in
up)
	if ! $DOCKER container inspect $PG >/dev/null 2>&1; then
		$DOCKER run -d --rm --name $PG -p 55432:5432 -e POSTGRES_PASSWORD=atomicfga \
			postgres:17 -c max_connections=500 >/dev/null
	fi
	if ! $DOCKER container inspect $MY >/dev/null 2>&1; then
		$DOCKER run -d --rm --name $MY -p 53306:3306 -e MYSQL_ROOT_PASSWORD=atomicfga \
			mysql:8 --max-connections=500 >/dev/null
	fi
	i=0
	until $DOCKER exec $PG pg_isready -U postgres >/dev/null 2>&1 &&
		$DOCKER exec $MY mysql -uroot -patomicfga -e 'SELECT 1' >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 120 ] || { echo "databases did not become ready" >&2; exit 1; }
		sleep 1
	done
	echo "postgres: postgres://postgres:atomicfga@localhost:55432/postgres?sslmode=disable"
	echo "mysql:    root:atomicfga@tcp(localhost:53306)/"
	;;
down)
	$DOCKER rm -f $PG $MY >/dev/null 2>&1 || true
	;;
*)
	echo "usage: $0 [up|down]" >&2
	exit 2
	;;
esac
