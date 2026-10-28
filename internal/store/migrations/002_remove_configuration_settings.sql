-- Web configuration belongs to the TOML file. Keep only operational
-- settings such as processing_paused in SQLite.
DELETE FROM settings WHERE key IN (
    'sui_base_url',
    'sui_book_id',
    'sui_book_name',
    'default_account',
    'openrouter_model',
    'sui_cookie',
    'openrouter_api_key'
);
