CREATE TABLE IF NOT EXISTS llm_model_catalog (
    cache_key TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    base_url TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    fetched_at TEXT NOT NULL
);

INSERT OR IGNORE INTO llm_model_catalog(cache_key, provider, base_url, payload_json, fetched_at)
SELECT 'openrouter|' || 'https://openrouter.ai/api/v1',
       'openrouter',
       'https://openrouter.ai/api/v1',
       payload_json,
       fetched_at
FROM openrouter_model_catalog
WHERE id = 1;
