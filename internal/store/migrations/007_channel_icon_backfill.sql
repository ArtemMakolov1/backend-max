-- Existing channels connected before visual metadata was copied can be
-- repaired from the bot's authenticated chat inventory. Only official HTTPS
-- MAX/OneMe asset hosts are accepted; unsafe legacy values remain empty and
-- are also rejected by the API/UI validation layers.
UPDATE channels AS connected
SET icon_url = observed.icon_url,
    updated_at = GREATEST(connected.updated_at, observed.last_seen_at)
FROM observed_bot_chats AS observed
WHERE connected.max_chat_id = observed.max_chat_id
  AND connected.icon_url = ''
  AND observed.icon_url <> ''
  AND length(observed.icon_url) <= 4096
  AND observed.icon_url !~ '[[:cntrl:]]'
  AND position('#' in observed.icon_url) = 0
  AND lower(observed.icon_url) ~ '^https://([a-z0-9-]+\.)*(max\.ru|oneme\.ru)/';
