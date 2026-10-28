-- Success explanations are already retained in entry_events.
UPDATE entries
SET error_code = '', error_message = '', version = version + 1
WHERE state = 'committed' AND (error_code <> '' OR error_message <> '');
