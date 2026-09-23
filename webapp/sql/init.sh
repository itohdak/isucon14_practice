#!/usr/bin/env bash

set -eux
cd $(dirname $0)

if [ "${ENV:-}" == "local-dev" ]; then
  exit 0
fi

if test -f /home/isucon/env.sh; then
	. /home/isucon/env.sh
fi

ISUCON_DB_HOST=${ISUCON_DB_HOST:-127.0.0.1}
ISUCON_DB_PORT=${ISUCON_DB_PORT:-3306}
ISUCON_DB_USER=${ISUCON_DB_USER:-isucon}
ISUCON_DB_PASSWORD=${ISUCON_DB_PASSWORD:-isucon}
ISUCON_DB_NAME=${ISUCON_DB_NAME:-isuride}

# MySQLを初期化
mysql -u"$ISUCON_DB_USER" \
		-p"$ISUCON_DB_PASSWORD" \
		--host "$ISUCON_DB_HOST" \
		--port "$ISUCON_DB_PORT" \
		"$ISUCON_DB_NAME" < 1-schema.sql

mysql -u"$ISUCON_DB_USER" \
		-p"$ISUCON_DB_PASSWORD" \
		--host "$ISUCON_DB_HOST" \
		--port "$ISUCON_DB_PORT" \
		"$ISUCON_DB_NAME" < 2-master-data.sql

gzip -dkc 3-initial-data.sql.gz | mysql -u"$ISUCON_DB_USER" \
		-p"$ISUCON_DB_PASSWORD" \
		--host "$ISUCON_DB_HOST" \
		--port "$ISUCON_DB_PORT" \
		"$ISUCON_DB_NAME"

mysql -u"$ISUCON_DB_USER" \
		-p"$ISUCON_DB_PASSWORD" \
		--host "$ISUCON_DB_HOST" \
		--port "$ISUCON_DB_PORT" \
		"$ISUCON_DB_NAME" <<'SQL'
ALTER TABLE chairs
  ADD COLUMN total_distance INTEGER NOT NULL DEFAULT 0 COMMENT '累計移動距離',
  ADD COLUMN total_distance_updated_at DATETIME(6) NULL COMMENT '累計移動距離の最終更新日時',
  ADD COLUMN latest_latitude INTEGER NULL COMMENT '直近の緯度',
  ADD COLUMN latest_longitude INTEGER NULL COMMENT '直近の経度',
  -- Denormalized "chair has no unfinished assigned ride" flag, maintained
  -- transactionally by matchOneRide (set FALSE on match) and
  -- chairGetNotification (set TRUE only when the COMPLETED status is the
  -- one being delivered), instead of recomputed via a correlated NOT
  -- EXISTS scan on every matching tick. Defaults to TRUE so a
  -- newly-created chair (no assigned rides yet) is immediately
  -- matchable, matching the old query's behavior.
  ADD COLUMN is_free BOOLEAN NOT NULL DEFAULT TRUE COMMENT '新しいライドを受付可能か';

UPDATE chairs
LEFT JOIN (
  SELECT chair_id,
         SUM(IFNULL(distance, 0)) AS total_distance,
         MAX(created_at) AS total_distance_updated_at
  FROM (
    SELECT chair_id,
           created_at,
           ABS(latitude - LAG(latitude) OVER (PARTITION BY chair_id ORDER BY created_at)) +
           ABS(longitude - LAG(longitude) OVER (PARTITION BY chair_id ORDER BY created_at)) AS distance
    FROM chair_locations
  ) tmp
  GROUP BY chair_id
) distance_table ON distance_table.chair_id = chairs.id
LEFT JOIN (
  SELECT chair_id, latitude, longitude
  FROM chair_locations
  WHERE (chair_id, created_at) IN (
    SELECT chair_id, MAX(created_at) FROM chair_locations GROUP BY chair_id
  )
) latest_location ON latest_location.chair_id = chairs.id
SET chairs.total_distance = IFNULL(distance_table.total_distance, 0),
    chairs.total_distance_updated_at = distance_table.total_distance_updated_at,
    chairs.latest_latitude = latest_location.latitude,
    chairs.latest_longitude = latest_location.longitude,
    chairs.updated_at = chairs.updated_at;

-- One-time backfill using the exact same semantics as the NOT EXISTS
-- query this flag replaces, so historical seed data (which may include
-- pre-assigned rides) starts with a correct is_free value.
UPDATE chairs
SET is_free = NOT EXISTS (
  SELECT 1
  FROM rides assigned_rides
  WHERE assigned_rides.chair_id = chairs.id
    AND NOT EXISTS (
      SELECT 1
      FROM ride_statuses completed_statuses
      WHERE completed_statuses.ride_id = assigned_rides.id
        AND completed_statuses.status = 'COMPLETED'
        AND completed_statuses.chair_sent_at IS NOT NULL
    )
),
    updated_at = updated_at;
SQL
