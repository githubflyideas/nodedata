-- ClickHouse schema for nodedata
-- Run this once to initialize the database.

CREATE DATABASE IF NOT EXISTS nodedata;

CREATE TABLE IF NOT EXISTS nodedata.samples
(
    host      LowCardinality(String),
    metric_id LowCardinality(String),
    ts        DateTime      CODEC(DoubleDelta, ZSTD(1)),
    value     Float64       CODEC(Gorilla, ZSTD(1)),
    z         Array(Int8)   CODEC(Delta, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (host, metric_id, ts)
TTL ts + INTERVAL 60 DAY;

CREATE TABLE IF NOT EXISTS nodedata.sigma
(
    host        LowCardinality(String),
    metric_id   LowCardinality(String),
    lag         LowCardinality(String),
    hour        UInt8,
    sigma       Float64,
    n_samples   UInt32,
    computed_on Date
)
ENGINE = ReplacingMergeTree(computed_on)
ORDER BY (host, metric_id, lag, hour);
