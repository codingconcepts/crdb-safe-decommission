# crdb-safe-decommission

Explores some of the queries you can use to ensure a safe node decommission.

### Setup

Start the first 3 nodes, wait for each to come up, then initialise the first 3 nodes.

Starting just the first 3 shows how system ranges prefer a replication factor (RF) of 5, regardless of node count.

```sh
for i in 1 2 3; do
  docker compose -f compose.yml up -d node$i
done

for i in 1 2 3; do
  until docker exec node$i curl -sf http://localhost:8080/health >/dev/null 2>&1; do
    sleep 1
  done
done

docker exec node1 cockroach init --insecure
```

Configure cluster

```sh
docker exec -it node1 cockroach sql --insecure --set "prompt1=%/>"
```

```sql
SET allow_unsafe_internals = true;

SET CLUSTER SETTING kv.rangefeed.enabled = true;

-- Lowering from default to make for a quicker demo. Not recommended for real clusters.
SET CLUSTER SETTING kv.replication_reports.interval = '00:00:10';
```

Show effective replication factors to see the RF=5 for the system tables. This is expected.

```sql
SELECT
  z.target,
  regexp_extract(z.full_config_sql, 'num_replicas = (\d+)')::INT8 AS rf
FROM crdb_internal.zones z ORDER BY rf DESC, target;
```

Serial start of the remaining 2 nodes, so node IDs match container names.

```sh
for i in 4 5; do
  docker compose -f compose.yml up -d node$i
done
```

Build the changefeed sink. It runs in a `FROM scratch` Linux container, so cross-compile - a host build gives `exec /sink: exec format error`.

```sh
(cd sink && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOHOSTARCH) go build -o sink .)
```

Start the load balancer and our CDC sink.

```sh
docker compose -f compose.yml up -d haproxy sink
```

Baseline. Refreshes on `kv.replication_reports.interval` (1m default, overrideen to 10s).

Run and wait for both to drop to 0.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

0 here only because 5 nodes satisfy the RF=5 of the system tables.

Initialise and run a workload, leaving it running in another terminal.

```sh
cockroach workload init bank --drop "postgresql://root@localhost:26257?sslmode=disable"

cockroach workload run bank \
--max-rate 100 \
--tolerate-errors \
--duration 24h "postgresql://root@localhost:26257?sslmode=disable"
```

Start changefeed job

```sql
CREATE CHANGEFEED FOR TABLE bank.public.bank
INTO 'file-http://sink:8081/workload'
WITH
  resolved='1s',
  min_checkpoint_frequency='1s',
  initial_scan='no';
```

Monitor

```sh
docker logs sink -f
```

`replication_stats` is keyed by zone, not database. Use `SHOW RANGES` and count **live** voters.

```sql
WITH live AS (
  SELECT node_id FROM crdb_internal.gossip_nodes JOIN crdb_internal.gossip_liveness USING (node_id)
  WHERE is_live AND membership = 'active'
)
SELECT live_voters, COUNT(*) AS ranges
FROM (SELECT (SELECT COUNT(*) FROM unnest(voting_replicas) v WHERE v IN (SELECT node_id FROM live)) AS live_voters
      FROM [SHOW RANGES FROM DATABASE bank])
GROUP BY live_voters ORDER BY live_voters;
```

### The reversible window

Drain and stop the node, we can still rollback after this.

```sh
docker exec node1 cockroach node drain 5 --insecure --drain-wait=60s

docker stop node5
```

Under-replication shows after ~1m.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

Some ranges now have 2 live_voters instead of three.

```sql
WITH live AS (
  SELECT node_id FROM crdb_internal.gossip_nodes JOIN crdb_internal.gossip_liveness USING (node_id)
  WHERE is_live AND membership = 'active'
)
SELECT live_voters, COUNT(*) AS ranges
FROM (SELECT (SELECT COUNT(*) FROM unnest(voting_replicas) v WHERE v IN (SELECT node_id FROM live)) AS live_voters
      FROM [SHOW RANGES FROM DATABASE bank])
GROUP BY live_voters ORDER BY live_voters;
```

### The controlled-failure test

Second node down kills quorum on ~30% of ranges. Still recoverable - both nodes were stopped, not decommissioned, so the stores and node IDs survive.

```sh
docker stop node4
```

Unavailable ranges start to appear.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

Start node5 - same ID, same store, workload resumes with node4 still down.

```sh
docker start node5
```

Unavailable ranges disappear.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

Then start node4.

```sh
docker start node4
```

Underreplicated ranges appear.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

Wait for both underreplicated and unavailable to drop to 0.

### Decommission

Decommission node 5.

```sh
docker exec node1 cockroach node decommission 5 --insecure --wait=all
```

Read membership from a node that isn't retiring.

```sql
SELECT node_id, membership FROM crdb_internal.gossip_liveness ORDER BY node_id;
```

Note that ranges will appear underreplicated; this is just the system ranges.

```sql
SELECT
  sum(under_replicated_ranges)::INT8 AS underreplicated,
  sum(unavailable_ranges)::INT8 AS unavailable
FROM system.replication_stats;
```

User data won't be underreplicated.

```sql
WITH live AS (
  SELECT node_id FROM crdb_internal.gossip_nodes JOIN crdb_internal.gossip_liveness USING (node_id)
  WHERE is_live AND membership = 'active'
)
SELECT live_voters, COUNT(*) AS ranges
FROM (SELECT (SELECT COUNT(*) FROM unnest(voting_replicas) v WHERE v IN (SELECT node_id FROM live)) AS live_voters
      FROM [SHOW RANGES FROM DATABASE bank])
GROUP BY live_voters ORDER BY live_voters;
```

Test all ranges with a read against probe_ranges. Each read needs a valid lease, so it proves reachability.

```sql
SELECT
  COUNT(*) FILTER (WHERE error != '') AS errors,
  COUNT(*) AS ranges_probed
FROM crdb_internal.probe_ranges(INTERVAL '10s', 'read');
```

Test all ranges with a write against probe_ranges. The write goes through raft, so it needs a live quorum (something you'll need to care about after a decommission). A read probe can pass on a range that has already lost the ability to commit, for as long as its lease holds.

```sql
SELECT
  COUNT(*) FILTER (WHERE error != '') AS errors,
  COUNT(*) AS ranges_probed
FROM crdb_internal.probe_ranges(INTERVAL '10s', 'write');
```

It writes to each range and deletes it again in the same transaction, so nothing is left behind.

This isn't a free operation and will take longer the more ranges you have.

Check nobody else is stuck mid-decommission.

```sql
SELECT
  node_id,
  membership
FROM crdb_internal.gossip_liveness
WHERE membership = 'decommissioning';
```

### The retirement gate

The retirement_gate function takes a node ID (in our case 5) and runs a series of queries to determine whether it's safe to turn off.

```sql
CREATE OR REPLACE FUNCTION retirement_gate(target_node INT8)
RETURNS TABLE (seq INT8, check_name STRING, status STRING, detail STRING)
LANGUAGE SQL
AS $$
  WITH
  live AS (
    SELECT n.node_id, n.locality
    FROM crdb_internal.gossip_nodes n
    JOIN crdb_internal.gossip_liveness l USING (node_id)
    WHERE n.is_live AND l.membership = 'active'
  ),
  zone_target AS (
    SELECT zone_id, subzone_id, target,
           regexp_extract(full_config_sql, 'num_replicas = (\d+)')::INT8 AS rf
    FROM crdb_internal.zones
  ),
  prefixes AS (
    SELECT nl.node_id,
           (SELECT string_agg(x, ',' ORDER BY ord)
            FROM unnest(string_to_array(nl.locality, ',')) WITH ORDINALITY AS u(x, ord)
            WHERE ord <= i) AS loc
    FROM live nl, generate_series(1, array_length(string_to_array(nl.locality, ','), 1)) AS i
  ),
  locs AS (
    SELECT loc, depth, count(*) OVER (PARTITION BY depth) AS siblings
    FROM (SELECT loc, length(loc) - length(replace(loc, ',', '')) AS depth FROM prefixes GROUP BY loc)
  ),
  r AS (
    SELECT range_id, voting_replicas, replicas, lease_holder,
           array_length(voting_replicas, 1) AS voters,
           div(array_length(voting_replicas, 1), 2) + 1 AS quorum,
           (SELECT count(*) FROM unnest(voting_replicas) AS v JOIN live ON live.node_id = v) AS live_voters
    FROM crdb_internal.ranges
  )
  SELECT * FROM (
    SELECT 1::INT8 AS seq, 'membership' AS check_name,
           CASE WHEN (SELECT membership FROM crdb_internal.gossip_liveness WHERE node_id = target_node) = 'decommissioned'
                THEN 'PASS' ELSE 'FAIL' END AS status,
           'n' || target_node::STRING || ' = ' ||
           coalesce((SELECT membership FROM crdb_internal.gossip_liveness WHERE node_id = target_node), 'absent') AS detail

    UNION ALL SELECT 2, 'no other node decommissioning',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg('n' || node_id::STRING, ','), 'none')
    FROM crdb_internal.gossip_liveness
    WHERE membership = 'decommissioning' AND node_id != target_node

    UNION ALL SELECT 3, 'replicas on target',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END, count(*)::STRING || ' ranges'
    FROM r WHERE target_node = ANY r.replicas

    UNION ALL SELECT 4, 'leases on target',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END, count(*)::STRING || ' leases'
    FROM r WHERE r.lease_holder = target_node

    UNION ALL SELECT 5, 'unavailable ranges',
      CASE WHEN coalesce(sum(unavailable_ranges), 0) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(sum(unavailable_ranges), 0)::STRING
    FROM system.replication_stats

    UNION ALL SELECT 6, 'under-replicated',
      CASE WHEN count(*) FILTER (WHERE fixable) > 0 THEN 'FAIL'
           WHEN count(*) > 0                        THEN 'WARN'
           ELSE 'PASS' END,
      coalesce(string_agg(
        target || ' ' || u::STRING ||
        CASE WHEN fixable THEN '' ELSE ' (rf ' || rf::STRING || ' > ' || nodes::STRING || ' live nodes)' END,
        ', ' ORDER BY target), 'none')
    FROM (
      SELECT coalesce(zt.target, 'zone ' || s.zone_id::STRING) AS target,
             s.under_replicated_ranges AS u,
             coalesce(zt.rf, 0) AS rf,
             (SELECT count(*) FROM live) AS nodes,
             coalesce(zt.rf, 0) <= (SELECT count(*) FROM live) AS fixable
      FROM system.replication_stats s
      LEFT JOIN zone_target zt ON zt.zone_id = s.zone_id AND zt.subzone_id = s.subzone_id
    ) WHERE u > 0

    UNION ALL SELECT 7, 'locality single points of failure',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(loc || ' (' || lost::STRING || ')', ', '), 'none')
    FROM (
      SELECT p.loc, p.siblings,
             count(*) FILTER (
               WHERE r.voters - (SELECT count(*) FROM unnest(r.voting_replicas) AS v
                                 WHERE v IN (SELECT node_id FROM prefixes WHERE loc = p.loc)) < r.quorum
             ) AS lost
      FROM locs p CROSS JOIN r GROUP BY p.loc, p.siblings
    ) WHERE siblings > 1 AND lost > 0

    UNION ALL SELECT 8, 'constraint violations',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(config || ' (' || violating_ranges::STRING || ')', ', '), 'none')
    FROM system.replication_constraint_stats WHERE violating_ranges > 0

    UNION ALL SELECT 9, 'ranges with zero failure headroom',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END, count(*)::STRING || ' ranges at quorum edge'
    FROM r WHERE live_voters - quorum < 1

    UNION ALL SELECT 10, 'survives next node failure',
      CASE WHEN coalesce(max(lost), 0) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg('n' || node_id::STRING || ':' || lost::STRING, ' ' ORDER BY node_id), 'n/a')
    FROM (
      SELECT l.node_id,
             count(*) FILTER (WHERE r.live_voters - 1 < r.quorum AND l.node_id = ANY r.voting_replicas) AS lost
      FROM live l CROSS JOIN r GROUP BY l.node_id
    )

    UNION ALL SELECT 11, 'sql sessions on target',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(DISTINCT split_part(client_address, ':', 1), ', '), 'none')
    FROM crdb_internal.cluster_sessions WHERE node_id = target_node

    UNION ALL SELECT 12, 'sql conns on target',
      CASE WHEN coalesce(max(c), 0) = 0 THEN 'PASS' ELSE 'FAIL' END, coalesce(max(c), 0)::STRING
    FROM (SELECT (metrics->>'sql.conns')::INT8 AS c FROM crdb_internal.kv_node_status WHERE node_id = target_node)

    UNION ALL SELECT 13, 'jobs coordinated by target',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(job_type || ' ' || job_id::STRING, ', '), 'none')
    FROM crdb_internal.jobs
    WHERE coordinator_id = target_node AND status IN ('running', 'paused', 'pause-requested', 'reverting')

    UNION ALL SELECT 14, 'jobs referencing target address',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(job_type || ' ' || job_id::STRING, ', '), 'none')
    FROM crdb_internal.jobs
    WHERE status NOT IN ('succeeded', 'canceled', 'failed')
      AND (description ILIKE '%nodelocal://' || target_node::STRING || '/%'
        OR description ILIKE '%node' || target_node::STRING || ':%')

    UNION ALL SELECT 15, 'changefeed lag',
      CASE WHEN coalesce(max(lag), INTERVAL '0s') < INTERVAL '1 minute' THEN 'PASS' ELSE 'FAIL' END,
      coalesce(string_agg(job_id::STRING || ' ' || lag::STRING, ', '), 'no running changefeeds')
    FROM (
      SELECT job_id, now() - to_timestamp(high_water_timestamp::FLOAT8 / 1e9) AS lag
      FROM crdb_internal.jobs
      WHERE job_type = 'CHANGEFEED' AND status = 'running' AND high_water_timestamp IS NOT NULL
    )

    UNION ALL SELECT 16, 'changefeed aggregators on target',
      CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END, count(*)::STRING || ' flows'
    FROM crdb_internal.cluster_distsql_flows f WHERE f.stmt = '' AND f.node_id = target_node

    UNION ALL SELECT 17, 'client activity on target',
      CASE WHEN a.age IS NULL                 THEN 'PASS'
           WHEN a.age < INTERVAL '10 seconds' THEN 'FAIL'
           WHEN a.age < INTERVAL '5 minutes'  THEN 'WARN'
           ELSE 'PASS' END,
      concat_ws('; ',
        CASE WHEN a.live_age IS NOT NULL THEN 'in-flight query ' || a.live_age::STRING || ' ago' END,
        coalesce(a.sampled, 'no client sessions sampled'))
    FROM (
      SELECT
        least(
          (SELECT min(now() - sample_time)
           FROM crdb_internal.cluster_active_session_history
           WHERE node_id = target_node AND workload_type = 'STATEMENT'
             AND coalesce(app_name, '') NOT LIKE '$ internal%'),
          (SELECT min(now() - active_query_start)
           FROM crdb_internal.cluster_sessions
           WHERE node_id = target_node AND active_query_start IS NOT NULL
             AND coalesce(application_name, '') NOT LIKE '$ internal%')
        ) AS age,
        (SELECT min(now() - active_query_start)
         FROM crdb_internal.cluster_sessions
         WHERE node_id = target_node AND active_query_start IS NOT NULL
           AND coalesce(application_name, '') NOT LIKE '$ internal%') AS live_age,
        (SELECT string_agg(app || ' ' || age::STRING || ' ago', ', ' ORDER BY age)
         FROM (
           SELECT coalesce(nullif(app_name, ''), '(unnamed app)') AS app,
                  min(now() - sample_time) AS age
           FROM crdb_internal.cluster_active_session_history
           WHERE node_id = target_node AND workload_type = 'STATEMENT'
             AND coalesce(app_name, '') NOT LIKE '$ internal%'
           GROUP BY 1
         )) AS sampled
    ) a
  ) ORDER BY seq;
$$;
```

Then call it per node, from a node that isn't retiring:

```sql
SELECT * FROM retirement_gate(5);
```

Note that under-replicated will flag a warning. This is expected, as the system ranges (especially liveness and meta) want RF=5.

Show the number of warnings and failures.

```sql
SELECT COUNT(*) FILTER (WHERE status = 'FAIL') AS fails,
       COUNT(*) FILTER (WHERE status = 'WARN') AS warns
FROM retirement_gate(5);
```

Alternatively, here are the same seventeen checks in isolation. Target node is the literal `5` throughout.

```sql
-- 1. membership.
SELECT CASE WHEN membership = 'decommissioned' THEN 'PASS' ELSE 'FAIL' END AS status,
       'n5 = ' || membership AS detail
FROM crdb_internal.gossip_liveness WHERE node_id = 5;

-- 2. no other node decommissioning.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg('n' || node_id::STRING, ','), 'none') AS detail
FROM crdb_internal.gossip_liveness WHERE membership = 'decommissioning' AND node_id != 5;

-- 3. replicas on target.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       COUNT(*)::STRING || ' ranges' AS detail
FROM crdb_internal.ranges WHERE 5 = ANY replicas;

-- 4. leases on target.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       COUNT(*)::STRING || ' leases' AS detail
FROM crdb_internal.ranges WHERE lease_holder = 5;

-- 5. unavailable ranges.
SELECT CASE WHEN coalesce(sum(unavailable_ranges), 0) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(sum(unavailable_ranges), 0)::STRING AS detail
FROM system.replication_stats;

-- 6. under-replicated.
WITH live AS (
  SELECT n.node_id FROM crdb_internal.gossip_nodes n
  JOIN crdb_internal.gossip_liveness l USING (node_id)
  WHERE n.is_live AND l.membership = 'active'
)
SELECT CASE WHEN COUNT(*) FILTER (WHERE fixable) > 0 THEN 'FAIL'
            WHEN COUNT(*) > 0                        THEN 'WARN'
            ELSE 'PASS' END AS status,
       coalesce(string_agg(
         target || ' ' || u::STRING ||
         CASE WHEN fixable THEN '' ELSE ' (rf ' || rf::STRING || ' > ' || nodes::STRING || ' live nodes)' END,
         ', ' ORDER BY target), 'none') AS detail
FROM (
  SELECT coalesce(z.target, 'zone ' || s.zone_id::STRING) AS target,
         s.under_replicated_ranges AS u,
         coalesce(regexp_extract(z.full_config_sql, 'num_replicas = (\d+)')::INT8, 0) AS rf,
         (SELECT COUNT(*) FROM live) AS nodes,
         coalesce(regexp_extract(z.full_config_sql, 'num_replicas = (\d+)')::INT8, 0)
           <= (SELECT COUNT(*) FROM live) AS fixable
  FROM system.replication_stats s
  LEFT JOIN crdb_internal.zones z ON z.zone_id = s.zone_id AND z.subzone_id = s.subzone_id
) WHERE u > 0;

-- 7. locality single points of failure.
WITH live AS (
  SELECT n.node_id, n.locality FROM crdb_internal.gossip_nodes n
  JOIN crdb_internal.gossip_liveness l USING (node_id)
  WHERE n.is_live AND l.membership = 'active'
),
prefixes AS (
  SELECT nl.node_id,
         (SELECT string_agg(x, ',' ORDER BY ord)
          FROM unnest(string_to_array(nl.locality, ',')) WITH ORDINALITY AS u(x, ord)
          WHERE ord <= i) AS loc
  FROM live nl, generate_series(1, array_length(string_to_array(nl.locality, ','), 1)) AS i
),
locs AS (
  SELECT loc, depth, COUNT(*) OVER (PARTITION BY depth) AS siblings
  FROM (SELECT loc, length(loc) - length(replace(loc, ',', '')) AS depth FROM prefixes GROUP BY loc)
),
r AS (
  SELECT voting_replicas, array_length(voting_replicas, 1) AS voters,
         div(array_length(voting_replicas, 1), 2) + 1 AS quorum
  FROM crdb_internal.ranges
)
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(loc || ' (' || lost::STRING || ')', ', '), 'none') AS detail
FROM (
  SELECT p.loc, p.siblings,
         COUNT(*) FILTER (
           WHERE r.voters - (SELECT COUNT(*) FROM unnest(r.voting_replicas) AS v
                             WHERE v IN (SELECT node_id FROM prefixes WHERE loc = p.loc)) < r.quorum
         ) AS lost
  FROM locs p CROSS JOIN r GROUP BY p.loc, p.siblings
) WHERE siblings > 1 AND lost > 0;

-- 8. constraint violations.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(config || ' (' || violating_ranges::STRING || ')', ', '), 'none') AS detail
FROM system.replication_constraint_stats WHERE violating_ranges > 0;

-- 9. ranges with zero failure headroom.
WITH live AS (
  SELECT n.node_id FROM crdb_internal.gossip_nodes n
  JOIN crdb_internal.gossip_liveness l USING (node_id)
  WHERE n.is_live AND l.membership = 'active'
),
r AS (
  SELECT div(array_length(voting_replicas, 1), 2) + 1 AS quorum,
         (SELECT COUNT(*) FROM unnest(voting_replicas) AS v JOIN live ON live.node_id = v) AS live_voters
  FROM crdb_internal.ranges
)
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       COUNT(*)::STRING || ' ranges at quorum edge' AS detail
FROM r WHERE live_voters - quorum < 1;

-- 10. survives next node failure.
WITH live AS (
  SELECT n.node_id FROM crdb_internal.gossip_nodes n
  JOIN crdb_internal.gossip_liveness l USING (node_id)
  WHERE n.is_live AND l.membership = 'active'
),
r AS (
  SELECT voting_replicas, div(array_length(voting_replicas, 1), 2) + 1 AS quorum,
         (SELECT COUNT(*) FROM unnest(voting_replicas) AS v JOIN live ON live.node_id = v) AS live_voters
  FROM crdb_internal.ranges
)
SELECT CASE WHEN coalesce(max(lost), 0) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg('n' || node_id::STRING || ':' || lost::STRING, ' ' ORDER BY node_id), 'n/a') AS detail
FROM (
  SELECT l.node_id,
         COUNT(*) FILTER (WHERE r.live_voters - 1 < r.quorum AND l.node_id = ANY r.voting_replicas) AS lost
  FROM live l CROSS JOIN r GROUP BY l.node_id
);

-- 11. sql sessions on target.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(DISTINCT split_part(client_address, ':', 1), ', '), 'none') AS detail
FROM crdb_internal.cluster_sessions WHERE node_id = 5;

-- 12. sql conns on target.
SELECT CASE WHEN coalesce(max(c), 0) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(max(c), 0)::STRING AS detail
FROM (SELECT (metrics->>'sql.conns')::INT8 AS c FROM crdb_internal.kv_node_status WHERE node_id = 5);

-- 13. jobs coordinated by target.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(job_type || ' ' || job_id::STRING, ', '), 'none') AS detail
FROM crdb_internal.jobs
WHERE coordinator_id = 5 AND status IN ('running', 'paused', 'pause-requested', 'reverting');

-- 14. jobs referencing target address.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(job_type || ' ' || job_id::STRING, ', '), 'none') AS detail
FROM crdb_internal.jobs
WHERE status NOT IN ('succeeded', 'canceled', 'failed')
  AND (description ILIKE '%nodelocal://5/%' OR description ILIKE '%node5:%');

-- 15. changefeed lag.
SELECT CASE WHEN coalesce(max(lag), INTERVAL '0s') < INTERVAL '1 minute' THEN 'PASS' ELSE 'FAIL' END AS status,
       coalesce(string_agg(job_id::STRING || ' ' || lag::STRING, ', '), 'no running changefeeds') AS detail
FROM (
  SELECT job_id, now() - to_timestamp(high_water_timestamp::FLOAT8 / 1e9) AS lag
  FROM crdb_internal.jobs
  WHERE job_type = 'CHANGEFEED' AND status = 'running' AND high_water_timestamp IS NOT NULL
);

-- 16. changefeed aggregators on target.
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS status,
       COUNT(*)::STRING || ' flows' AS detail
FROM crdb_internal.cluster_distsql_flows WHERE stmt = '' AND node_id = 5;

-- 17. client activity on target.
SELECT CASE WHEN a.age IS NULL                 THEN 'PASS'
            WHEN a.age < INTERVAL '10 seconds' THEN 'FAIL'
            WHEN a.age < INTERVAL '5 minutes'  THEN 'WARN'
            ELSE 'PASS' END AS status,
       concat_ws('; ',
         CASE WHEN a.live_age IS NOT NULL THEN 'in-flight query ' || a.live_age::STRING || ' ago' END,
         coalesce(a.sampled, 'no client sessions sampled')) AS detail
FROM (
  SELECT
    least(
      (SELECT min(now() - sample_time) FROM crdb_internal.cluster_active_session_history
       WHERE node_id = 5 AND workload_type = 'STATEMENT' AND coalesce(app_name, '') NOT LIKE '$ internal%'),
      (SELECT min(now() - active_query_start) FROM crdb_internal.cluster_sessions
       WHERE node_id = 5 AND active_query_start IS NOT NULL
         AND coalesce(application_name, '') NOT LIKE '$ internal%')
    ) AS age,
    (SELECT min(now() - active_query_start) FROM crdb_internal.cluster_sessions
     WHERE node_id = 5 AND active_query_start IS NOT NULL
       AND coalesce(application_name, '') NOT LIKE '$ internal%') AS live_age,
    (SELECT string_agg(app || ' ' || age::STRING || ' ago', ', ' ORDER BY age)
     FROM (
       SELECT coalesce(nullif(app_name, ''), '(unnamed app)') AS app, min(now() - sample_time) AS age
       FROM crdb_internal.cluster_active_session_history
       WHERE node_id = 5 AND workload_type = 'STATEMENT'
         AND coalesce(app_name, '') NOT LIKE '$ internal%'
       GROUP BY 1
     )) AS sampled
) a;
```

### The load balancer

Going through HAProxy, the authoritative answer is HAProxy's, not CockroachDB's:

```sh
docker exec haproxy wget -qO- "http://127.0.0.1:8081/;csv" \
  | awk -F, '$1=="cockroach-sql" && $2!="FRONTEND" && $2!="BACKEND" {print $1","$2","$18","$19}'
```

We expect to seee `DOWN` for node5. `/health?ready=1` starts to fail once the node drains.

### Teardown

```sh
make teardown
```
