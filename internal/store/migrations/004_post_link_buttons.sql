ALTER TABLE posts
    ADD COLUMN link_buttons JSONB DEFAULT '[]'::jsonb;

UPDATE posts
SET link_buttons = '[]'::jsonb
WHERE link_buttons IS NULL;

ALTER TABLE posts
    ADD CONSTRAINT posts_link_buttons_shape CHECK (
        link_buttons IS NOT NULL
        AND
        jsonb_typeof(link_buttons) = 'array'
        AND jsonb_array_length(link_buttons) <= 3
    ) NOT VALID;

ALTER TABLE posts
    VALIDATE CONSTRAINT posts_link_buttons_shape;
