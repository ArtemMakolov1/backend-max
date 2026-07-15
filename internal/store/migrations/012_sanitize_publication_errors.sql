-- Provider and protocol details belong in server logs, not in user-facing
-- publication history. Replace legacy raw MAX failures already stored before
-- the application started persisting safe messages.
UPDATE posts
SET last_error = 'MAX не смог опубликовать пост. Проверьте подключение канала и попробуйте ещё раз.'
WHERE last_error LIKE 'MAX API error%'
   OR last_error LIKE '%errors.send-message.%'
   OR last_error LIKE '%proto.payload%';

UPDATE posts
SET last_error = 'Предыдущая публикация была прервана. Проверьте канал перед повторной попыткой.'
WHERE last_error = 'Previous publication was interrupted; check the MAX channel before retrying to avoid a duplicate post.';
